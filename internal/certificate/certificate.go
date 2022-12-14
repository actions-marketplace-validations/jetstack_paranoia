// SPDX-License-Identifier: Apache-2.0

package certificate

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Found is a single X.509 certificate which was found by a parser inside the
// given image.
type Found struct {
	// Location is the filepath location where the certificate was found.
	Location string

	// Parser is the name of the parser which discovered the certificate.
	Parser string

	// Certificate is the parsed certificate. May be nil if the parser failed to
	// decode a found certificate.
	Certificate *x509.Certificate

	// Fingerprint is the SHA-1 fingerprint of the certificate.
	FingerprintSha1 [20]byte

	// Fingerprint is the SHA-256 fingerprint of the certificate.
	FingerprintSha256 [32]byte
}

// Partial is a "partial" certificate. Usually the result of parsing something that looks like a certificate but isn't
// valid, or some other anomaly. These are often worthy of further investigation, but aren't compatible with Paranoia's
// various certificate operations.
type Partial struct {
	// Location is the filepath location where the certificate was found.
	Location string

	// Parser is the name of the parser which discovered the certificate.
	Parser string

	// Reason is a human-readable explanation of the certificate, either describe
	// why it couldn't be parsed or a summary of the parsed certificate.
	Reason string
}

type rseekerOpener func() (io.ReadSeeker, error)

type ParsedCertificates struct {
	// Found is a slice of full, valid certificates we've found in the given container image.
	Found []Found
	// Partials is a slice of any partial certificates we've found. This might be fragments of certificates in memory
	// or other anomalies.
	Partials []Partial
}

func (p *ParsedCertificates) appendParsed(q *ParsedCertificates) {
	p.Found = append(p.Found, q.Found...)
	p.Partials = append(p.Partials, q.Partials...)
}

// parser is the interface implemented by X.509 certificate parsers.
type parser interface {
	Find(context.Context, string, rseekerOpener) (*ParsedCertificates, error)
}

// FindCertificates will scan a container image, given as a file handler to a TAR file, for certificates and return them.
func FindCertificates(ctx context.Context, imageTar io.Reader) (*ParsedCertificates, error) {
	var (
		parsers = []parser{pem{}}
		parsed  = &ParsedCertificates{}
	)

	tz := tar.NewReader(imageTar)

	for {
		header, err := tz.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return nil, err
		}

		// If file is not a regular file, ignore.
		if header.Typeflag != tar.TypeReg {
			continue
		}

		opener, oCleanup, err := openerForFile(ctx, header, tz)
		if err != nil {
			return nil, err
		}

		var (
			wg   sync.WaitGroup
			lock sync.Mutex
			errs []string
		)

		wg.Add(len(parsers))

		// Run all parsers.
		for _, p := range parsers {
			go func(p parser) {
				defer wg.Done()
				parserParsed, err := p.Find(ctx, filepath.Join("/", header.Name), opener)
				lock.Lock()
				defer lock.Unlock()
				if err != nil {
					errs = append(errs, err.Error())
				}
				parsed.appendParsed(parserParsed)
			}(p)
		}

		wg.Wait()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if err := oCleanup(); err != nil {
			errs = append(errs, err.Error())
		}

		if len(errs) > 0 {
			return parsed, fmt.Errorf("parser error finding certificates: %s", strings.Join(errs, "; "))
		}
	}

	return parsed, nil
}

// openerForFile returns an rseekerOpener and clean-up function for the given
// tarball file. Depending of the size of the file, the ReadSeeker will
// ordinate from an in-memory buffer, or a temporary file.
func openerForFile(ctx context.Context, header *tar.Header, reader io.Reader) (rseekerOpener, func() error, error) {
	// If file is larger than a Gig, write to a temporary file.
	if header.Size > (1 << 30) {
		tmp, err := os.CreateTemp(os.TempDir(), strings.ReplaceAll(filepath.Clean(header.Name), string(filepath.Separator), "-"))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create temporary file: %w", err)
		}

		if _, err := io.Copy(tmp, reader); err != nil {
			return nil, nil, fmt.Errorf("failed to write image file to temporary file: %w", err)
		}

		if err := tmp.Close(); err != nil {
			return nil, nil, fmt.Errorf("failed to close temporary file: %w", err)
		}

		return func() (io.ReadSeeker, error) {
			return os.Open(tmp.Name())
		}, func() error { return os.Remove(tmp.Name()) }, nil
	} else {
		// Simple in-memory buffer.
		ff, err := io.ReadAll(reader)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read image file: %w", err)
		}
		return func() (io.ReadSeeker, error) {
			return bytes.NewReader(ff), nil
		}, func() error { return nil }, nil
	}
}
