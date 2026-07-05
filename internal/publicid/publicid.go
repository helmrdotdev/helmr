package publicid

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	randomBytes  = 16
	randomLength = 26
)

var (
	randomEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)
	randomPattern  = regexp.MustCompile(`^[a-z2-7]{26}$`)

	ErrInvalidPrefix = errors.New("invalid public id prefix")
	ErrInvalidFormat = errors.New("invalid public id format")
)

type Prefix string

const (
	Organization      Prefix = "org_"
	User              Prefix = "usr_"
	Invitation        Prefix = "inv_"
	APIKey            Prefix = "apk_"
	Project           Prefix = "prj_"
	Environment       Prefix = "env_"
	Deployment        Prefix = "dep_"
	Task              Prefix = "task_"
	DeploymentTask    Prefix = "dtask_"
	Sandbox           Prefix = "sbx_"
	Schedule          Prefix = "sch_"
	Workspace         Prefix = "wsp_"
	WorkspaceVersion  Prefix = "wsv_"
	Session           Prefix = "ses_"
	SessionRun        Prefix = "srun_"
	Run               Prefix = "run_"
	RunOperation      Prefix = "rop_"
	Wait              Prefix = "wait_"
	Stream            Prefix = "str_"
	StreamRecord      Prefix = "srec_"
	Token             Prefix = "tok_"
	PublicAccessToken Prefix = "pat_"
)

var registeredPrefixes = []Prefix{
	Organization,
	User,
	Invitation,
	APIKey,
	Project,
	Environment,
	Deployment,
	Task,
	DeploymentTask,
	Sandbox,
	Schedule,
	Workspace,
	WorkspaceVersion,
	Session,
	SessionRun,
	Run,
	RunOperation,
	Wait,
	Stream,
	StreamRecord,
	Token,
	PublicAccessToken,
}

var prefixSet = func() map[Prefix]struct{} {
	set := make(map[Prefix]struct{}, len(registeredPrefixes))
	for _, prefix := range registeredPrefixes {
		set[prefix] = struct{}{}
	}
	return set
}()

func RegisteredPrefixes() []Prefix {
	prefixes := make([]Prefix, len(registeredPrefixes))
	copy(prefixes, registeredPrefixes)
	return prefixes
}

func (p Prefix) String() string {
	return string(p)
}

func (p Prefix) Valid() bool {
	_, ok := prefixSet[p]
	return ok
}

func (p Prefix) Regexp() (string, error) {
	if !p.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidPrefix, p)
	}
	return "^" + regexp.QuoteMeta(string(p)) + "[a-z2-7]{26}$", nil
}

func New(prefix Prefix) (string, error) {
	return NewWithReader(prefix, rand.Reader)
}

func NewWithReader(prefix Prefix, reader io.Reader) (string, error) {
	if !prefix.Valid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidPrefix, prefix)
	}
	var entropy [randomBytes]byte
	if _, err := io.ReadFull(reader, entropy[:]); err != nil {
		return "", fmt.Errorf("generate public id entropy: %w", err)
	}
	random := strings.ToLower(randomEncoding.EncodeToString(entropy[:]))
	return string(prefix) + random, nil
}

func Parse(id string) (Prefix, string, error) {
	id = strings.TrimSpace(id)
	prefixEnd := strings.IndexByte(id, '_')
	if prefixEnd <= 0 {
		return "", "", fmt.Errorf("%w: missing prefix", ErrInvalidFormat)
	}
	prefix := Prefix(id[:prefixEnd+1])
	if !prefix.Valid() {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidPrefix, prefix)
	}
	random := id[prefixEnd+1:]
	if !randomPattern.MatchString(random) {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidFormat, id)
	}
	return prefix, random, nil
}

func Validate(id string) error {
	_, _, err := Parse(id)
	return err
}

func ValidateFor(prefix Prefix, id string) error {
	if !prefix.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidPrefix, prefix)
	}
	actual, _, err := Parse(id)
	if err != nil {
		return err
	}
	if actual != prefix {
		return fmt.Errorf("%w: expected %s got %s", ErrInvalidPrefix, prefix, actual)
	}
	return nil
}
