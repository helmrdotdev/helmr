package checkpoint

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	magic         = "helmr-checkpoint-aesgcm-v0\n"
	chunkSize     = 4 << 20
	maxNonceSize  = 64
	maxSealedSize = chunkSize + 1024
)

type Encryptor struct {
	aead cipher.AEAD
	rand io.Reader
}

func New(key []byte) (*Encryptor, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("configure checkpoint encryption key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("configure checkpoint cipher: %w", err)
	}
	return &Encryptor{aead: aead, rand: rand.Reader}, nil
}

func KeyFromBase64(raw string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("decode checkpoint encryption key: %w", err)
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("checkpoint encryption key must decode to 32 bytes, got %d", len(decoded))
	}
	return decoded, nil
}

func (c *Encryptor) Encrypt(ctx context.Context, plaintext io.Reader, ciphertext io.Writer, purpose string) error {
	if c == nil {
		return errors.New("checkpoint encryptor is required")
	}
	if _, err := io.WriteString(ciphertext, magic); err != nil {
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
		if n == 0 && readErr == io.EOF {
			return c.writeEndRecord(ciphertext, purpose, chunk)
		}
		nonce := make([]byte, c.aead.NonceSize())
		if _, err := io.ReadFull(c.rand, nonce); err != nil {
			return fmt.Errorf("generate checkpoint nonce: %w", err)
		}
		sealed := c.aead.Seal(nil, nonce, buffer[:n], additionalData(purpose, chunk))
		if err := writeRecord(ciphertext, nonce, sealed); err != nil {
			return err
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			return c.writeEndRecord(ciphertext, purpose, chunk+1)
		}
	}
}

func (c *Encryptor) Decrypt(ctx context.Context, ciphertext io.Reader, plaintext io.Writer, purpose string) error {
	if c == nil {
		return errors.New("checkpoint encryptor is required")
	}
	prefix := make([]byte, len(magic))
	if _, err := io.ReadFull(ciphertext, prefix); err != nil {
		return fmt.Errorf("read checkpoint header: %w", err)
	}
	if string(prefix) != magic {
		return errors.New("unsupported checkpoint encryption format")
	}
	for chunk := uint64(0); ; chunk++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		nonce, sealed, err := readRecord(ciphertext)
		if err != nil {
			return err
		}
		opened, err := c.aead.Open(nil, nonce, sealed, additionalData(purpose, chunk))
		if err != nil {
			return fmt.Errorf("decrypt checkpoint chunk %d: %w", chunk, err)
		}
		if len(opened) == 0 {
			return requireCiphertextEOF(ciphertext)
		}
		if _, err := plaintext.Write(opened); err != nil {
			return err
		}
	}
}

func requireCiphertextEOF(r io.Reader) error {
	var extra [1]byte
	n, err := r.Read(extra[:])
	if n > 0 {
		return errors.New("trailing checkpoint ciphertext after end record")
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read checkpoint end: %w", err)
	}
	return errors.New("checkpoint ciphertext did not end after end record")
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

func (c *Encryptor) writeEndRecord(w io.Writer, purpose string, chunk uint64) error {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(c.rand, nonce); err != nil {
		return fmt.Errorf("generate checkpoint end nonce: %w", err)
	}
	return writeRecord(w, nonce, c.aead.Seal(nil, nonce, nil, additionalData(purpose, chunk)))
}

func readRecord(r io.Reader) ([]byte, []byte, error) {
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, nil, err
	}
	nonceSize := binary.BigEndian.Uint32(header[:4])
	sealedSize := binary.BigEndian.Uint32(header[4:])
	if nonceSize == 0 || nonceSize > maxNonceSize || sealedSize == 0 || sealedSize > maxSealedSize {
		return nil, nil, errors.New("invalid checkpoint chunk header")
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

func additionalData(purpose string, chunk uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], chunk)
	return append([]byte(purpose+"\x00"), encoded[:]...)
}
