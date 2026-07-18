//go:build linux

package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	verifierIdentity       = "helmr-verifier"
	verifierIdentityMax    = 1 << 20
	verifierRoot           = "/tmp"
	verifierOldRoot        = "/.helmr-old-root"
	verifierSecureNoRoot   = 1 << 0
	verifierSecureNoRootL  = 1 << 1
	verifierSecureNoFixup  = 1 << 2
	verifierSecureNoFixupL = 1 << 3
)

type verifierFDReader struct {
	fd int
}

func (reader verifierFDReader) ReadAt(destination []byte, offset int64) (int, error) {
	total := 0
	for total < len(destination) {
		count, err := unix.Pread(reader.fd, destination[total:], offset+int64(total))
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		total += count
		if err != nil {
			return total, err
		}
		if count == 0 {
			return total, io.EOF
		}
	}
	return total, nil
}

type verifierFDWriter struct {
	fd int
}

func (writer verifierFDWriter) Write(source []byte) (int, error) {
	for {
		count, err := unix.Write(writer.fd, source)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return count, err
	}
}

func runVerifierChild(job verifierJob) (returnErr error) {
	os.Clearenv()
	defer func() {
		if err := unix.Close(verifierResultFD); err != nil &&
			!errors.Is(err, syscall.EBADF) {
			returnErr = errors.Join(returnErr, fmt.Errorf("close verifier result: %w", err))
		}
	}()

	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return errors.New("artifact verifier did not start as namespaced root PID 1")
	}
	uid, gid, err := resolveVerifierIdentity()
	if err != nil {
		return err
	}
	if err := validateVerifierDescriptors(job, nil); err != nil {
		return err
	}
	if err := isolateVerifierRoot(); err != nil {
		return err
	}
	if err := applyVerifierIdentity(uid, gid); err != nil {
		return err
	}
	if err := closeVerifierAmbientDescriptors(job); err != nil {
		return err
	}
	if err := validateVerifierDescriptors(job, &uid); err != nil {
		return err
	}

	// Artifact bytes must not be read before root isolation and privilege removal complete.
	result := verifierFDWriter{fd: verifierResultFD}
	if err := writeVerifierReady(result); err != nil {
		return err
	}
	return executeVerifierJob(job, result)
}

func executeVerifierJob(job verifierJob, result io.Writer) (returnErr error) {
	defer func() {
		if recover() != nil {
			if err := writeVerifierFailed(result, "artifact verifier panicked"); err != nil {
				returnErr = err
			} else {
				returnErr = nil
			}
		}
	}()

	canonical, err := verifyVerifierJob(context.Background(), job)
	if err == nil {
		return writeVerifierVerified(result, job, canonical)
	}
	kind, diagnostic := classifyVerifierError(job, err)
	if kind == verifierFailed {
		return writeVerifierFailed(result, diagnostic)
	}
	return writeVerifierInvalid(result, diagnostic)
}

func verifyVerifierJob(ctx context.Context, job verifierJob) ([]byte, error) {
	switch job {
	case programVerifierJob:
		return verifyProgramDescriptorPair(
			ctx,
			verifierArtifactBaseFD,
			verifierArtifactBaseFD+1,
		)
	case runtimeVerifierJob:
		return verifyRuntimeDescriptor(ctx, verifierArtifactBaseFD)
	default:
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("artifact verifier job = %q", job),
		}
	}
}

func verifyProgramDescriptorPair(ctx context.Context, codeFD, dependencyFD int) ([]byte, error) {
	code, err := artifactFromDescriptor(
		ctx,
		codeFD,
		codeArtifact,
		ProgramCodeArtifactMediaType,
	)
	if err != nil {
		return nil, fmt.Errorf("code Artifact: %w", err)
	}
	dependencies, err := artifactFromDescriptor(
		ctx,
		dependencyFD,
		dependencyArtifact,
		ProgramDependencyArtifactMediaType,
	)
	if err != nil {
		return nil, fmt.Errorf("dependency Artifact: %w", err)
	}

	verified, err := verifyProgramArtifacts(ctx, programArtifacts{
		Code:         code,
		Dependencies: dependencies,
	})
	if err != nil {
		return nil, err
	}
	canonical, err := CanonicalProgramIndex(verified.Index())
	if err != nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("canonicalize verified Program index: %w", err),
		}
	}
	return canonical, nil
}

func verifyRuntimeDescriptor(ctx context.Context, fd int) ([]byte, error) {
	artifact, err := artifactFromDescriptor(
		ctx,
		fd,
		runtimeArtifact,
		RuntimeArtifactMediaType,
	)
	if err != nil {
		return nil, fmt.Errorf("runtime Artifact: %w", err)
	}
	index, err := verifyRuntimeArtifact(ctx, artifact)
	if err != nil {
		return nil, err
	}
	canonical, err := CanonicalRuntimeIndex(index)
	if err != nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("canonicalize verified Runtime index: %w", err),
		}
	}
	return canonical, nil
}

func artifactFromDescriptor(
	ctx context.Context,
	fd int,
	role artifactRole,
	mediaType string,
) (programArtifact, error) {
	if err := checkSquashFSContext(ctx); err != nil {
		return programArtifact{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return programArtifact{}, &artifactInfrastructureError{
			cause: fmt.Errorf("stat Artifact descriptor: %w", err),
		}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size < 0 {
		return programArtifact{}, &artifactInfrastructureError{
			cause: errors.New("Artifact descriptor is not a regular file"),
		}
	}
	physicalLimit, err := artifactPhysicalLimit(role)
	if err != nil {
		return programArtifact{}, err
	}
	if stat.Size < 1 || stat.Size > physicalLimit {
		return programArtifact{}, &artifactContentError{
			cause: fmt.Errorf(
				"Artifact physical size = %d, want within [1,%d]",
				stat.Size,
				physicalLimit,
			),
		}
	}

	source := verifierFDReader{fd: fd}
	digest, err := digestVerifierDescriptor(ctx, source, stat.Size)
	if err != nil {
		return programArtifact{}, err
	}
	reader, err := newSquashFSArtifactReader(ctx, source, stat.Size, role)
	if err != nil {
		return programArtifact{}, err
	}
	return programArtifact{
		Digest:    "sha256:" + digest,
		SizeBytes: stat.Size,
		MediaType: mediaType,
		Reader:    reader,
	}, nil
}

func digestVerifierDescriptor(
	ctx context.Context,
	source io.ReaderAt,
	size int64,
) (string, error) {
	if size < 0 {
		return "", &artifactInfrastructureError{
			cause: fmt.Errorf("Artifact size = %d", size),
		}
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	for offset := int64(0); offset < size; {
		if err := checkSquashFSContext(ctx); err != nil {
			return "", err
		}
		length := int64(len(buffer))
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		if err := readSquashFSAt(source, buffer[:int(length)], offset); err != nil {
			return "", fmt.Errorf("digest Artifact: %w", err)
		}
		if _, err := hash.Write(buffer[:int(length)]); err != nil {
			return "", &artifactInfrastructureError{
				cause: fmt.Errorf("digest Artifact: %w", err),
			}
		}
		offset += length
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func classifyVerifierError(job verifierJob, err error) (verifierRecordKind, string) {
	var infrastructure *artifactInfrastructureError
	if errors.As(err, &infrastructure) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return verifierFailed, job.failedDiagnostic()
	}
	return verifierInvalid, job.invalidDiagnostic()
}

func resolveVerifierIdentity() (uint32, uint32, error) {
	passwd, err := readVerifierIdentityFile("/etc/passwd")
	if err != nil {
		return 0, 0, err
	}
	group, err := readVerifierIdentityFile("/etc/group")
	if err != nil {
		return 0, 0, err
	}
	uid, primaryGID, err := parseVerifierPasswd(passwd)
	if err != nil {
		return 0, 0, err
	}
	gid, err := parseVerifierGroup(group)
	if err != nil {
		return 0, 0, err
	}
	if primaryGID != gid {
		return 0, 0, errors.New("helmr-verifier passwd and group identities disagree")
	}
	return uid, gid, nil
}

func readVerifierIdentityFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open verifier identity file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, verifierIdentityMax+1))
	if err != nil {
		return nil, fmt.Errorf("read verifier identity file: %w", err)
	}
	if len(raw) > verifierIdentityMax {
		return nil, errors.New("verifier identity file exceeds its bound")
	}
	return raw, nil
}

func parseVerifierPasswd(raw []byte) (uint32, uint32, error) {
	var uid, gid uint32
	found := false
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		fields := bytes.Split(line, []byte{':'})
		if len(fields) == 0 || string(fields[0]) != verifierIdentity {
			continue
		}
		if found || len(fields) != 7 {
			return 0, 0, errors.New("helmr-verifier passwd entry is not unique and complete")
		}
		parsedUID, err := parseVerifierIdentityValue(fields[2])
		if err != nil {
			return 0, 0, fmt.Errorf("parse helmr-verifier UID: %w", err)
		}
		parsedGID, err := parseVerifierIdentityValue(fields[3])
		if err != nil {
			return 0, 0, fmt.Errorf("parse helmr-verifier primary GID: %w", err)
		}
		uid, gid, found = parsedUID, parsedGID, true
	}
	if !found {
		return 0, 0, errors.New("helmr-verifier passwd entry is missing")
	}
	return uid, gid, nil
}

func parseVerifierGroup(raw []byte) (uint32, error) {
	var gid uint32
	found := false
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		fields := bytes.Split(line, []byte{':'})
		if len(fields) == 0 || string(fields[0]) != verifierIdentity {
			continue
		}
		if found || len(fields) != 4 {
			return 0, errors.New("helmr-verifier group entry is not unique and complete")
		}
		parsed, err := parseVerifierIdentityValue(fields[2])
		if err != nil {
			return 0, fmt.Errorf("parse helmr-verifier group GID: %w", err)
		}
		gid, found = parsed, true
	}
	if !found {
		return 0, errors.New("helmr-verifier group entry is missing")
	}
	return gid, nil
}

func parseVerifierIdentityValue(raw []byte) (uint32, error) {
	value, err := strconv.ParseUint(string(raw), 10, 32)
	if err != nil || value == 0 {
		if err == nil {
			err = errors.New("identity is root")
		}
		return 0, err
	}
	return uint32(value), nil
}

func validateVerifierDescriptors(job verifierJob, forbiddenOwner *uint32) error {
	count := job.artifactCount()
	if count == 0 {
		return fmt.Errorf("artifact verifier job = %q", job)
	}
	for index := 0; index < count; index++ {
		if err := validateVerifierArtifactDescriptor(
			verifierArtifactBaseFD+index,
			forbiddenOwner,
		); err != nil {
			return fmt.Errorf("%s Artifact descriptor %d: %w", job, index, err)
		}
	}
	flags, err := unix.FcntlInt(uintptr(verifierResultFD), unix.F_GETFL, 0)
	if err != nil {
		return fmt.Errorf("inspect verifier result descriptor: %w", err)
	}
	if flags&unix.O_ACCMODE != unix.O_WRONLY {
		return errors.New("verifier result descriptor is not write-only")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(verifierResultFD, &stat); err != nil {
		return fmt.Errorf("stat verifier result descriptor: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFIFO {
		return errors.New("verifier result descriptor is not a pipe")
	}
	return nil
}

func validateVerifierArtifactDescriptor(fd int, forbiddenOwner *uint32) error {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return errors.New("descriptor is not read-only")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("descriptor is not a regular file")
	}
	if stat.Mode&0o222 != 0 {
		return errors.New("snapshot inode retains write permission")
	}
	if forbiddenOwner != nil && stat.Uid == *forbiddenOwner {
		return errors.New("snapshot inode is owned by the verifier identity")
	}
	return nil
}

func isolateVerifierRoot() error {
	var stat unix.Stat_t
	if err := unix.Lstat(verifierRoot, &stat); err != nil {
		return fmt.Errorf("stat verifier root mount point: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("verifier root mount point is not a directory")
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make verifier mount namespace private: %w", err)
	}
	if err := unix.Mount(
		"tmpfs",
		verifierRoot,
		"tmpfs",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC,
		"mode=0755,size=1048576,nr_inodes=16",
	); err != nil {
		return fmt.Errorf("mount verifier root: %w", err)
	}
	putOld := verifierRoot + verifierOldRoot
	if err := unix.Mkdir(putOld, 0o700); err != nil {
		return fmt.Errorf("create verifier old root: %w", err)
	}
	if err := unix.PivotRoot(verifierRoot, putOld); err != nil {
		return fmt.Errorf("pivot verifier root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir verifier root: %w", err)
	}
	if err := unix.Unmount(verifierOldRoot, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount verifier old root: %w", err)
	}
	if err := unix.Rmdir(verifierOldRoot); err != nil {
		return fmt.Errorf("remove verifier old root: %w", err)
	}
	if err := unix.Mount(
		"",
		"/",
		"",
		unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC,
		"",
	); err != nil {
		return fmt.Errorf("remount verifier root read-only: %w", err)
	}
	return nil
}

func applyVerifierIdentity(uid, gid uint32) error {
	if uid == 0 || gid == 0 {
		return errors.New("artifact verifier identity must be unprivileged")
	}
	if err := verifierAllThreadsPrctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set verifier no_new_privs: %w", err)
	}
	secureBits := verifierSecureNoRoot |
		verifierSecureNoRootL |
		verifierSecureNoFixup |
		verifierSecureNoFixupL
	if err := verifierAllThreadsPrctl(
		unix.PR_SET_SECUREBITS,
		uintptr(secureBits),
		0,
		0,
		0,
	); err != nil {
		return fmt.Errorf("lock verifier securebits: %w", err)
	}
	if err := verifierAllThreadsPrctl(
		unix.PR_CAP_AMBIENT,
		unix.PR_CAP_AMBIENT_CLEAR_ALL,
		0,
		0,
		0,
	); err != nil {
		return fmt.Errorf("clear verifier ambient capabilities: %w", err)
	}
	lastCapability, err := dropVerifierBoundingCapabilities()
	if err != nil {
		return err
	}
	if err := syscall.Setgroups(nil); err != nil {
		return fmt.Errorf("clear verifier supplementary groups: %w", err)
	}
	if err := syscall.Setresgid(int(gid), int(gid), int(gid)); err != nil {
		return fmt.Errorf("set verifier GID: %w", err)
	}
	if err := syscall.Setresuid(int(uid), int(uid), int(uid)); err != nil {
		return fmt.Errorf("set verifier UID: %w", err)
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := verifierAllThreadsCapset(&header, &data[0]); err != nil {
		return fmt.Errorf("clear verifier capabilities: %w", err)
	}
	return validateVerifierIdentity(uid, gid, lastCapability)
}

func verifierAllThreadsPrctl(
	option int,
	argument2 uintptr,
	argument3 uintptr,
	argument4 uintptr,
	argument5 uintptr,
) error {
	_, _, errno := syscall.AllThreadsSyscall6(
		syscall.SYS_PRCTL,
		uintptr(option),
		argument2,
		argument3,
		argument4,
		argument5,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func dropVerifierBoundingCapabilities() (uintptr, error) {
	const capabilitySearchLimit = 1024
	for capability := uintptr(0); capability < capabilitySearchLimit; capability++ {
		err := verifierAllThreadsPrctl(
			unix.PR_CAPBSET_DROP,
			capability,
			0,
			0,
			0,
		)
		if errors.Is(err, syscall.EINVAL) {
			if capability == 0 {
				return 0, errors.New("kernel exposes no capability bounding set")
			}
			return capability - 1, nil
		}
		if err != nil {
			return 0, fmt.Errorf("drop verifier capability %d: %w", capability, err)
		}
	}
	return 0, fmt.Errorf(
		"kernel capability domain reaches search limit %d",
		capabilitySearchLimit,
	)
}

func verifierAllThreadsCapset(
	header *unix.CapUserHeader,
	data *unix.CapUserData,
) error {
	_, _, errno := syscall.AllThreadsSyscall(
		syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(header)),
		uintptr(unsafe.Pointer(data)),
		0,
	)
	runtime.KeepAlive(header)
	runtime.KeepAlive(data)
	if errno != 0 {
		return errno
	}
	return nil
}

func validateVerifierIdentity(uid, gid uint32, lastCapability uintptr) error {
	ruid, euid, suid := unix.Getresuid()
	rgid, egid, sgid := unix.Getresgid()
	if ruid != int(uid) || euid != int(uid) || suid != int(uid) ||
		rgid != int(gid) || egid != int(gid) || sgid != int(gid) {
		return errors.New("artifact verifier identity did not become permanent")
	}
	noNewPrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil || noNewPrivileges != 1 {
		return errors.New("artifact verifier no_new_privs is not active")
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return fmt.Errorf("read verifier capabilities: %w", err)
	}
	for _, word := range data {
		if word.Effective != 0 || word.Permitted != 0 || word.Inheritable != 0 {
			return errors.New("artifact verifier retained process capabilities")
		}
	}
	for capability := uintptr(0); capability <= lastCapability; capability++ {
		present, err := unix.PrctlRetInt(unix.PR_CAPBSET_READ, capability, 0, 0, 0)
		if err != nil || present != 0 {
			return fmt.Errorf("artifact verifier retained bounding capability %d", capability)
		}
	}
	return nil
}

func closeVerifierAmbientDescriptors(job verifierJob) error {
	if err := unix.CloseRange(0, 2, 0); err != nil {
		return fmt.Errorf("close verifier standard descriptors: %w", err)
	}
	firstAmbient := verifierArtifactBaseFD + job.artifactCount()
	if err := unix.CloseRange(uint(firstAmbient), ^uint(0), 0); err != nil {
		return fmt.Errorf("close verifier ambient descriptors: %w", err)
	}
	for fd := 0; fd <= 2; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, syscall.EBADF) {
			return fmt.Errorf("verifier standard descriptor %d remains open", fd)
		}
	}
	for fd := verifierResultFD; fd < firstAmbient; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			return fmt.Errorf("verifier contract descriptor %d is closed: %w", fd, err)
		}
	}
	return nil
}
