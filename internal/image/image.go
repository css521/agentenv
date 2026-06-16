// Package image extracts a rootfs into a destination directory. There are two
// supported sources:
//
//   - SeedDir(src, dst, exclude): copy a local directory tree into dst.
//     Used by `init --from <dir|/>` — the production path that wraps an
//     existing container fs into a managed rootfs.
//   - MaterializeTarball(src, dst): extract a .tar / .tar.gz from a local
//     path or http(s) URL. Used by `init --tarball <path>` for demos and
//     air-gapped setups.
//
// Both run on stdlib only and apply the traversal-safe extractTar / walk
// logic in this package — there is no separate "pull from a registry" path
// because that would either need go-containerregistry's heavy dep tree or a
// fragile HTML-scraping mirror crawler. Users who want a container image
// should `docker export` it to a tarball and pass it via --tarball.
package image

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// httpClient is used only by MaterializeTarball when src is an http(s) URL.
// It carries the timeouts that bare http.Get lacks: a slow mirror or hung
// connection would otherwise block init forever.
var httpClient = &http.Client{
	Timeout: 10 * time.Minute,
	Transport: &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		DisableKeepAlives:     true,
	},
}

// MaterializeTarball extracts a .tar or .tar.gz at src (filesystem path or
// http/https URL) into destRootfs, which must already exist. The tarball stream
// goes through extractTar's traversal-safe handling (zip-slip / symlink-plant
// / hardlink-escape).
func MaterializeTarball(src, destRootfs string) error {
	stream, closer, err := openTarballSource(src)
	if err != nil {
		return err
	}
	defer closer()

	// Detect gzip by file extension (cheap, deterministic). For ambiguous
	// inputs (no extension) we still try gzip first and fall back to raw tar.
	if strings.HasSuffix(strings.ToLower(src), ".tar.gz") || strings.HasSuffix(strings.ToLower(src), ".tgz") {
		gz, err := gzip.NewReader(stream)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close()
		return extractTar(gz, destRootfs)
	}
	return extractTar(stream, destRootfs)
}

// openTarballSource returns a reader and a closer for src. URL → http GET (with
// timeouts); path → os.Open. Caller MUST call closer.
func openTarballSource(src string) (io.Reader, func(), error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		resp, err := httpClient.Get(src)
		if err != nil {
			return nil, nil, fmt.Errorf("download %s: %w", src, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, nil, fmt.Errorf("download %s: HTTP %d", src, resp.StatusCode)
		}
		return resp.Body, func() { resp.Body.Close() }, nil
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { f.Close() }, nil
}

// SeedDir copies the tree at src into dst, skipping any path in exclude (and not
// descending into excluded directories). It is best-effort: unreadable files
// (e.g. root-owned files when running unprivileged) and special files (devices,
// FIFOs, sockets) are skipped rather than aborting — so seeding from "/" inside a
// container works even as a non-root user.
func SeedDir(src, dst string, exclude map[string]bool) error {
	src = filepath.Clean(src)
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // tolerate unreadable/disappearing entries (e.g. /proc races)
		}
		if exclude[path] {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			_ = os.MkdirAll(target, fi.Mode().Perm())
			// MkdirAll filters perms through umask and ignores special bits.
			// Chmod with full Mode() restores setgid (group-inherit) and sticky
			// (e.g. /tmp's 1777) which the seed-from-/ flow needs.
			_ = os.Chmod(target, fi.Mode())
		case fi.Mode()&os.ModeSymlink != 0:
			if link, e := os.Readlink(path); e == nil {
				_ = os.Remove(target)
				_ = os.Symlink(link, target)
			}
		case fi.Mode().IsRegular():
			_ = seedCopyFile(path, target, fi)
		}
		return nil
	})
}

func seedCopyFile(src, dst string, fi os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err // unreadable → skip
	}
	defer in.Close()
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	// Note: OpenFile's perm arg is filtered by umask AND is mode-and-permbits only.
	// Use a generic create perm, then Chmod with the FULL file mode below so the
	// special bits — setuid/setgid/sticky — survive. Without that, sudo / su /
	// passwd / ping lose setuid (root can't even sudo), /tmp loses its 1777
	// sticky bit, and apt's _apt sandbox user can't create temp files. This is
	// the seed-from-/ path used by Dockerfile.control, so missing these bits
	// silently breaks the headline "agent runs unmodified inside" flow.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, cpErr := io.Copy(out, in)
	clErr := out.Close()
	if cpErr == nil {
		cpErr = clErr
	}
	_ = os.Chmod(dst, fi.Mode()) // restore setuid/setgid/sticky + perm bits
	_ = os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	return cpErr
}

// extractTar unpacks a tar stream into dir, with traversal-safe handling of
// every entry kind. Each candidate write path is validated to be UNDER dir
// before any filesystem operation — this defeats the classic three attacks
// against tar extractors (agentenv runs `init` as root, so an attacker-crafted
// image without these checks would be a host-level RCE):
//
//   - zip-slip: `../../etc/passwd` as a regular-file name
//   - symlink target escape: a symlink that points outside dir
//   - hardlink source escape: a hardlink that names an absolute host path
//
// Character/block devices and FIFOs are skipped on purpose: the sandbox mounts
// a fresh /dev at runtime, so device nodes in the image are irrelevant.
func extractTar(r io.Reader, dir string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// safeJoin resolves rel against absDir and rejects any path that escapes it
	// (handles "..", absolute paths, and traversal via cleaned components).
	safeJoin := func(rel string) (string, error) {
		p := filepath.Join(absDir, filepath.Clean("/"+rel))
		if p != absDir && !strings.HasPrefix(p, absDir+string(os.PathSeparator)) {
			return "", fmt.Errorf("tar entry escapes destination: %q", rel)
		}
		return p, nil
	}

	// noPlantedSymlinkInParents refuses to write `target` if any of its parent
	// dir components is a SYMLINK (regardless of where that symlink points).
	// This is the actual defense against the "tarball plants symlink, then
	// writes through it to escape dst" attack: a symlink is just data, but
	// once one exists in the extracted tree, subsequent writes through it
	// would redirect outside dst. Real-world rootfs tarballs (e.g. Ubuntu)
	// contain plenty of legitimate absolute symlinks (`etc/alternatives/*`),
	// so we must allow symlink creation — we just refuse to follow them.
	noPlantedSymlinkInParents := func(target string) error {
		for p := filepath.Dir(target); p != absDir && len(p) > len(absDir); p = filepath.Dir(p) {
			if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("path traversal: parent %q of target is a symlink", p)
			}
		}
		return nil
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := noPlantedSymlinkInParents(target); err != nil {
				return err
			}
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := noPlantedSymlinkInParents(target); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			// O_NOFOLLOW: refuse to write through a symlink at the final component
			// (defense in depth; the planted-symlink check above already covers
			// the same path).
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			// Allow any link target string — symlinks are inert until followed,
			// and the planted-symlink check above is what prevents subsequent
			// writes from traversing them.
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			// Hardlinks share an inode, so the link source must stay inside dst
			// — otherwise we'd be linking host files into the extracted tree.
			src, err := safeJoin(hdr.Linkname)
			if err != nil {
				return fmt.Errorf("hardlink source escapes destination: %q", hdr.Linkname)
			}
			if err := noPlantedSymlinkInParents(target); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Link(src, target); err != nil {
				return err
			}
		default:
			// Skip char/block devices, FIFOs, sockets, etc.
			continue
		}
		// Best-effort ownership/mode/timestamps. Use hdr.FileInfo().Mode() so
		// special bits (setuid/setgid/sticky) survive — notably /tmp's 1777,
		// without which apt's _apt sandbox user cannot create temp files.
		_ = os.Lchown(target, hdr.Uid, hdr.Gid)
		if hdr.Typeflag != tar.TypeSymlink {
			_ = os.Chmod(target, hdr.FileInfo().Mode())
			_ = os.Chtimes(target, time.Now(), hdr.ModTime)
		}
	}
}
