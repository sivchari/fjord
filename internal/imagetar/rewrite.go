// Package imagetar rewrites repository references embedded in docker/OCI
// image tar archives, such as those produced by "docker save".
package imagetar

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// rewrittenMembers lists tar entry names whose content may contain
// repository references that need rewriting.
var rewrittenMembers = map[string]bool{
	"manifest.json": true,
	"index.json":    true,
}

// RewriteTags reads a tar stream from src and writes an equivalent tar
// stream to dst, applying replacements (old prefix -> new prefix) as plain
// string substitutions to the manifest.json and index.json members. Keys
// are applied in sorted order. All other members, including binary blobs,
// are copied byte-for-byte and in their original order. RewriteTags does
// not handle gzip compression; callers are responsible for that.
func RewriteTags(dst io.Writer, src io.Reader, replacements map[string]string) error {
	keys := make([]string, 0, len(replacements))
	for k := range replacements {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	tr := tar.NewReader(src)
	tw := tar.NewWriter(dst)

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("reading tar header: %w", err)
		}

		if rewrittenMembers[header.Name] {
			if err := writeRewrittenMember(tw, tr, header, replacements, keys); err != nil {
				return err
			}

			continue
		}

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("writing tar header for %s: %w", header.Name, err)
		}

		if _, err := io.CopyN(tw, tr, header.Size); err != nil {
			return fmt.Errorf("copying tar member %s: %w", header.Name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar writer: %w", err)
	}

	return nil
}

// writeRewrittenMember reads the current tar member fully into memory,
// applies replacements as string substitutions, and writes the rewritten
// member (with an updated header Size) to tw.
func writeRewrittenMember(tw *tar.Writer, tr *tar.Reader, header *tar.Header, replacements map[string]string, keys []string) error {
	data, err := io.ReadAll(tr)
	if err != nil {
		return fmt.Errorf("reading tar member %s: %w", header.Name, err)
	}

	content := string(data)
	for _, k := range keys {
		content = strings.ReplaceAll(content, k, replacements[k])
	}

	rewritten := []byte(content)
	header.Size = int64(len(rewritten))

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", header.Name, err)
	}

	if _, err := tw.Write(rewritten); err != nil {
		return fmt.Errorf("writing tar member %s: %w", header.Name, err)
	}

	return nil
}
