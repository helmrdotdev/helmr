package computerhost

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	magic         = "helmr-checkpoint-aesgcm-v1\n"
	chunkSize     = 4 << 20
	keyIDSize     = 16
	saltSize      = 32
	headerSize    = len(magic) + keyIDSize + saltSize
	maxRecords    = uint64(1) << 32
	nonceSize     = 12
	tagSize       = 16
	maxNonceSize  = 64
	maxSealedSize = chunkSize + 1024
)

type CheckpointEncryptor struct {
	key   []byte
	keyID [keyIDSize]byte
	rand  io.Reader
}

var ErrCheckpointKeyUnavailable = errors.New("checkpoint encryption key is unavailable")

var errInvalidCheckpoint = errors.New("checkpoint contents are invalid")

func invalidCheckpoint(err error) error { return fmt.Errorf("%w: %w", errInvalidCheckpoint, err) }

func checkpointCipherReadError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return invalidCheckpoint(err)
	}
	return err
}

func NewCheckpointEncryptor(key []byte) (*CheckpointEncryptor, error) {
	if _, err := aes.NewCipher(key); err != nil {
		return nil, fmt.Errorf("configure checkpoint encryption key: %w", err)
	}
	fingerprint := sha256.Sum256(append([]byte("helmr-checkpoint-key-id-v1\x00"), key...))
	c := &CheckpointEncryptor{key: append([]byte(nil), key...), rand: rand.Reader}
	copy(c.keyID[:], fingerprint[:keyIDSize])
	return c, nil
}

// KeyID is a non-secret, domain-separated fingerprint for operational diagnosis.
func (c *CheckpointEncryptor) KeyID() string { return fmt.Sprintf("%x", c.keyID[:]) }

// Every encryption attempt has an independent key, including retries before
// publication. Checkpoint identity and role are part of the derivation context.
func (c *CheckpointEncryptor) objectCipher(header []byte, purpose string) (cipher.AEAD, error) {
	info := append([]byte(magic), c.keyID[:]...)
	info = binary.BigEndian.AppendUint64(info, uint64(len(purpose)))
	info = append(info, purpose...)
	key, err := hkdf.Key(sha256.New, c.key, header[len(magic)+keyIDSize:], string(info), 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// EncryptedSize includes the header, data frames and authenticated end record.
func (c *CheckpointEncryptor) EncryptedSize(plaintextBytes int64) (int64, error) {
	if c == nil || len(c.key) == 0 || plaintextBytes < 0 {
		return 0, errors.New("invalid checkpoint encryption size request")
	}
	records := plaintextBytes / chunkSize
	if plaintextBytes%chunkSize != 0 {
		records++
	}
	records++
	if uint64(records) > maxRecords {
		return 0, errors.New("checkpoint exceeds per-object nonce limit")
	}
	overhead := int64(8 + nonceSize + tagSize)
	if plaintextBytes > math.MaxInt64-int64(headerSize) {
		return 0, errors.New("checkpoint encrypted size overflow")
	}
	base := plaintextBytes + int64(headerSize)
	if records > (math.MaxInt64-base)/overhead {
		return 0, errors.New("checkpoint encrypted size overflow")
	}
	return base + records*overhead, nil
}

func (c *CheckpointEncryptor) Encrypt(ctx context.Context, plaintext io.Reader, ciphertext io.Writer, purpose string) error {
	if c == nil || len(c.key) == 0 {
		return errors.New("checkpoint encryptor is required")
	}
	header := make([]byte, headerSize)
	copy(header, magic)
	copy(header[len(magic):], c.keyID[:])
	if _, err := io.ReadFull(c.rand, header[len(magic)+keyIDSize:]); err != nil {
		return fmt.Errorf("generate checkpoint salt: %w", err)
	}
	aead, err := c.objectCipher(header, purpose)
	if err != nil {
		return err
	}
	if _, err = ciphertext.Write(header); err != nil {
		return err
	}
	buffer := make([]byte, chunkSize)
	for chunk := uint64(0); ; chunk++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := io.ReadFull(plaintext, buffer)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		end := n == 0 && readErr == io.EOF
		// Reserve one distinct nonce for the authenticated end record.
		if chunk >= maxRecords || (!end && chunk == maxRecords-1) {
			return errors.New("checkpoint exceeds per-object nonce limit")
		}
		nonce := checkpointNonce(chunk)
		sealed := aead.Seal(nil, nonce, buffer[:n], additionalData(header, purpose, chunk, end))
		if err := writeRecord(ciphertext, nonce, sealed); err != nil {
			return err
		}
		if end {
			return nil
		}
	}
}

func (c *CheckpointEncryptor) Decrypt(ctx context.Context, ciphertext io.Reader, plaintext io.Writer, purpose string) error {
	if c == nil || len(c.key) == 0 {
		return errors.New("checkpoint encryptor is required")
	}
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(ciphertext, header); err != nil {
		return fmt.Errorf("read checkpoint header: %w", checkpointCipherReadError(err))
	}
	if string(header[:len(magic)]) != magic {
		return invalidCheckpoint(errors.New("unsupported checkpoint encryption format"))
	}
	if !bytes.Equal(header[len(magic):len(magic)+keyIDSize], c.keyID[:]) {
		return fmt.Errorf("%w: configured key id %x, checkpoint key id %x", ErrCheckpointKeyUnavailable, c.keyID[:], header[len(magic):len(magic)+keyIDSize])
	}
	aead, err := c.objectCipher(header, purpose)
	if err != nil {
		return err
	}
	for chunk := uint64(0); chunk < maxRecords; chunk++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		nonce, sealed, err := readRecord(ciphertext)
		if err != nil {
			return checkpointCipherReadError(err)
		}
		if !bytes.Equal(nonce, checkpointNonce(chunk)) {
			return invalidCheckpoint(errors.New("invalid checkpoint record nonce"))
		}
		end := len(sealed) == aead.Overhead()
		opened, err := aead.Open(nil, nonce, sealed, additionalData(header, purpose, chunk, end))
		if err != nil {
			return invalidCheckpoint(fmt.Errorf("decrypt checkpoint chunk %d: %w", chunk, err))
		}
		if end {
			return requireCiphertextEOF(ciphertext)
		}
		if len(opened) > chunkSize || chunk == maxRecords-1 {
			return invalidCheckpoint(errors.New("checkpoint exceeds record bounds"))
		}
		if _, err := plaintext.Write(opened); err != nil {
			return err
		}
	}
	return invalidCheckpoint(errors.New("checkpoint exceeds per-object nonce limit"))
}
func checkpointNonce(chunk uint64) []byte {
	nonce := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(nonce[4:], chunk)
	return nonce
}

func requireCiphertextEOF(r io.Reader) error {
	var extra [1]byte
	n, err := r.Read(extra[:])
	if n > 0 {
		return invalidCheckpoint(errors.New("trailing checkpoint ciphertext after end record"))
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read checkpoint end: %w", err)
	}
	return invalidCheckpoint(errors.New("checkpoint ciphertext did not end after end record"))
}

func writeRecord(w io.Writer, nonce []byte, sealed []byte) error {
	var header [8]byte
	binary.BigEndian.PutUint32(header[:4], uint32(len(nonce)))
	binary.BigEndian.PutUint32(header[4:], uint32(len(sealed)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if _, err := w.Write(nonce); err != nil {
		return err
	}
	_, err := w.Write(sealed)
	return err
}

func readRecord(r io.Reader) ([]byte, []byte, error) {
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, nil, err
	}
	nonceSize := binary.BigEndian.Uint32(header[:4])
	sealedSize := binary.BigEndian.Uint32(header[4:])
	if nonceSize == 0 || nonceSize > maxNonceSize || sealedSize == 0 || sealedSize > maxSealedSize {
		return nil, nil, invalidCheckpoint(errors.New("invalid checkpoint chunk header"))
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(r, nonce); err != nil {
		return nil, nil, err
	}
	sealed := make([]byte, sealedSize)
	if _, err := io.ReadFull(r, sealed); err != nil {
		return nil, nil, err
	}
	return nonce, sealed, nil
}

func additionalData(header []byte, purpose string, chunk uint64, end bool) []byte {
	data := append([]byte(nil), header...)
	data = binary.BigEndian.AppendUint64(data, uint64(len(purpose)))
	data = append(data, purpose...)
	data = binary.BigEndian.AppendUint64(data, chunk)
	if end {
		return append(data, 1)
	}
	return append(data, 0)
}
