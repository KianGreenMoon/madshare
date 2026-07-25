//go:build tools

// Command fbsdpkg builds a FreeBSD binary package (.pkg) out of a staged
// directory tree — on any OS, without pkg(8) and without a FreeBSD host.
//
// A .pkg is nothing but a compressed tar with two metadata members in front:
//
//	+COMPACT_MANIFEST   the package metadata (what a repository catalog holds)
//	+MANIFEST           the same plus files, directories and install scripts
//	/usr/local/bin/…    the payload, stored under ABSOLUTE paths, root:wheel
//
// so producing one is a matter of getting the manifest right. The shape
// implemented here was read off real packages from pkg.freebsd.org rather than
// guessed — in particular the two details that are easy to get wrong: every
// entry of "files" is an object (not a bare checksum string), and its "sum" is
// the sha256 hex prefixed with "1$" (pkg's checksum-format tag).
//
// The tar goes to stdout; the caller compresses it (zstd for a modern .pkg).
// Directories are NOT emitted as archive members: pkg creates the parents of
// every file on its own, and the "directories" manifest key is what gives an
// owned directory its ownership, its mode, and its removal on deinstall.
//
// Build-tagged `tools` so `go build ./...` and `go install ./...` never pick a
// packaging helper up — the same guard the mesh lab binaries use.
package main

import (
	"archive/tar"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// fileEntry is one row of the manifest's "files" map. Perm is an octal string
// ("0644"), Sum the "1$<sha256 hex>" form pkg records for `pkg check -s`.
type fileEntry struct {
	Sum   string `json:"sum"`
	Uname string `json:"uname"`
	Gname string `json:"gname"`
	Perm  string `json:"perm"`
	Mtime int64  `json:"mtime"`
}

// dirEntry is one row of "directories": a directory the package owns, created
// with this ownership even when it ships empty (the data dir), and removed on
// deinstall when nothing is left in it.
type dirEntry struct {
	Uname string `json:"uname"`
	Gname string `json:"gname"`
	Perm  string `json:"perm"`
}

// pkgMessage is a note pkg prints to the admin. Type "install" shows on a fresh
// install only, which is what post-install instructions want.
type pkgMessage struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
}

// manifest is +MANIFEST. +COMPACT_MANIFEST is the same value with the three
// payload-describing members dropped (see compact).
type manifest struct {
	Name         string       `json:"name"`
	Origin       string       `json:"origin"`
	Version      string       `json:"version"`
	Comment      string       `json:"comment"`
	Maintainer   string       `json:"maintainer"`
	WWW          string       `json:"www"`
	ABI          string       `json:"abi"`
	Arch         string       `json:"arch"`
	Prefix       string       `json:"prefix"`
	Flatsize     int64        `json:"flatsize"`
	LicenseLogic string       `json:"licenselogic"`
	Licenses     []string     `json:"licenses,omitempty"`
	Desc         string       `json:"desc"`
	Categories   []string     `json:"categories,omitempty"`
	Users        []string     `json:"users,omitempty"`
	Groups       []string     `json:"groups,omitempty"`
	Messages     []pkgMessage `json:"messages,omitempty"`

	Files       map[string]fileEntry `json:"files,omitempty"`
	Directories map[string]dirEntry  `json:"directories,omitempty"`
	Scripts     map[string]string    `json:"scripts,omitempty"`
}

// compact returns the +COMPACT_MANIFEST form: metadata only, no payload.
func (m manifest) compact() manifest {
	m.Files, m.Directories, m.Scripts = nil, nil, nil
	return m
}

// repeatable collects a flag given more than once.
type repeatable []string

func (r *repeatable) String() string     { return strings.Join(*r, ",") }
func (r *repeatable) Set(v string) error { *r = append(*r, v); return nil }

func main() {
	var (
		stage      = flag.String("stage", "", "staged root directory to package (required)")
		name       = flag.String("name", "", "package name (required)")
		version    = flag.String("version", "", "package version (required)")
		origin     = flag.String("origin", "", "ports-style origin, e.g. audio/madshare")
		abi        = flag.String("abi", "", "package ABI, e.g. FreeBSD:14:amd64 (required)")
		arch       = flag.String("arch", "", "legacy arch string, e.g. freebsd:14:x86:64 (required)")
		prefix     = flag.String("prefix", "/usr/local", "install prefix")
		comment    = flag.String("comment", "", "one-line summary (required)")
		descFile   = flag.String("desc-file", "", "file holding the long description (required)")
		msgFile    = flag.String("message-file", "", "file holding the post-install message")
		maintainer = flag.String("maintainer", "", "maintainer contact (required)")
		www        = flag.String("www", "", "project URL")
		license    = flag.String("license", "", "ports license id, e.g. AGPLv3")
		category   = flag.String("category", "", "ports category")
		user       = flag.String("user", "", "service user the package registers")
		group      = flag.String("group", "", "service group the package registers")
		mtime      = flag.Int64("mtime", time.Now().Unix(), "mtime stamped on every member")
		out        = flag.String("o", "-", "output tar path, or - for stdout")
	)
	var dirs, scripts repeatable
	flag.Var(&dirs, "dir", "owned directory as path:uname:gname:perm (repeatable)")
	flag.Var(&scripts, "script", "install script as <phase>=<file>, e.g. post-install=x.sh (repeatable)")
	flag.Parse()

	if err := run(runArgs{
		stage: *stage, name: *name, version: *version, origin: *origin,
		abi: *abi, arch: *arch, prefix: *prefix, comment: *comment,
		descFile: *descFile, msgFile: *msgFile, maintainer: *maintainer,
		www: *www, license: *license, category: *category,
		user: *user, group: *group, mtime: *mtime, out: *out,
		dirs: dirs, scripts: scripts,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "fbsdpkg:", err)
		os.Exit(1)
	}
}

type runArgs struct {
	stage, name, version, origin       string
	abi, arch, prefix, comment         string
	descFile, msgFile, maintainer, www string
	license, category, user, group     string
	mtime                              int64
	out                                string
	dirs, scripts                      repeatable
}

func run(a runArgs) error {
	for _, req := range []struct{ flag, val string }{
		{"-stage", a.stage}, {"-name", a.name}, {"-version", a.version},
		{"-abi", a.abi}, {"-arch", a.arch}, {"-comment", a.comment},
		{"-desc-file", a.descFile}, {"-maintainer", a.maintainer},
	} {
		if req.val == "" {
			return fmt.Errorf("%s is required", req.flag)
		}
	}

	desc, err := os.ReadFile(a.descFile)
	if err != nil {
		return err
	}
	m := manifest{
		Name: a.name, Origin: a.origin, Version: a.version,
		Comment: a.comment, Maintainer: a.maintainer, WWW: a.www,
		ABI: a.abi, Arch: a.arch, Prefix: a.prefix,
		LicenseLogic: "single",
		Desc:         strings.TrimRight(string(desc), "\n"),
		Files:        map[string]fileEntry{},
	}
	if a.license != "" {
		m.Licenses = []string{a.license}
	}
	if a.category != "" {
		m.Categories = []string{a.category}
	}
	if a.user != "" {
		m.Users = []string{a.user}
	}
	if a.group != "" {
		m.Groups = []string{a.group}
	}
	if a.msgFile != "" {
		msg, err := os.ReadFile(a.msgFile)
		if err != nil {
			return err
		}
		m.Messages = []pkgMessage{{Message: strings.TrimRight(string(msg), "\n"), Type: "install"}}
	}
	if len(a.dirs) > 0 {
		m.Directories = map[string]dirEntry{}
		for _, spec := range a.dirs {
			path, entry, err := parseDir(spec)
			if err != nil {
				return err
			}
			m.Directories[path] = entry
		}
	}
	if len(a.scripts) > 0 {
		m.Scripts = map[string]string{}
		for _, spec := range a.scripts {
			phase, file, ok := strings.Cut(spec, "=")
			if !ok {
				return fmt.Errorf("bad -script %q: want <phase>=<file>", spec)
			}
			body, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			m.Scripts[phase] = strings.TrimRight(string(body), "\n")
		}
	}

	payload, err := collect(a.stage, a.mtime)
	if err != nil {
		return err
	}
	if len(payload) == 0 {
		return fmt.Errorf("staged tree %s holds no files", a.stage)
	}
	for _, f := range payload {
		m.Files[f.path] = f.entry
		m.Flatsize += f.size
	}

	w := os.Stdout
	if a.out != "-" {
		f, err := os.Create(a.out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	buf := bufio.NewWriter(w)
	if err := writeArchive(buf, m, payload, a.mtime); err != nil {
		return err
	}
	return buf.Flush()
}

// parseDir reads a path:uname:gname:perm directory spec.
func parseDir(spec string) (string, dirEntry, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 4 {
		return "", dirEntry{}, fmt.Errorf("bad -dir %q: want path:uname:gname:perm", spec)
	}
	if _, err := strconv.ParseUint(parts[3], 8, 32); err != nil {
		return "", dirEntry{}, fmt.Errorf("bad -dir %q: %q is not an octal mode", spec, parts[3])
	}
	return parts[0], dirEntry{Uname: parts[1], Gname: parts[2], Perm: parts[3]}, nil
}

// staged is one payload file: where it lives now, where it installs to, and the
// manifest row describing it.
type staged struct {
	src   string
	path  string // absolute install path, the key in "files" and the tar name
	size  int64
	entry fileEntry
}

// collect walks the staged root and describes every regular file in it. Modes
// come from the staged tree (so the stage step decides 0555 vs 0644); ownership
// is always root:wheel, as in every real package — the service user owns its
// data directory, never the installed files.
func collect(root string, mtime int64) ([]staged, error) {
	var out []staged
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files can be packaged (found %s)", p, d.Type())
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum, err := sha256File(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, staged{
			src:  p,
			path: "/" + filepath.ToSlash(rel),
			size: info.Size(),
			entry: fileEntry{
				Sum:   "1$" + sum,
				Uname: "root",
				Gname: "wheel",
				Perm:  fmt.Sprintf("%04o", info.Mode().Perm()),
				Mtime: mtime,
			},
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeArchive emits the tar: the two manifests first (pkg reads them from the
// front of the stream without unpacking the payload), then the files.
func writeArchive(w io.Writer, m manifest, payload []staged, mtime int64) error {
	tw := tar.NewWriter(w)
	compact, err := json.Marshal(m.compact())
	if err != nil {
		return err
	}
	full, err := json.Marshal(m)
	if err != nil {
		return err
	}
	for _, meta := range []struct {
		name string
		body []byte
	}{{"+COMPACT_MANIFEST", compact}, {"+MANIFEST", full}} {
		if err := writeHeader(tw, meta.name, int64(len(meta.body)), 0o644, mtime); err != nil {
			return err
		}
		if _, err := tw.Write(meta.body); err != nil {
			return err
		}
	}
	for _, f := range payload {
		perm, err := strconv.ParseInt(f.entry.Perm, 8, 32)
		if err != nil {
			return err
		}
		if err := writeHeader(tw, f.path, f.size, perm, mtime); err != nil {
			return err
		}
		if err := copyFile(tw, f.src, f.size); err != nil {
			return err
		}
	}
	return tw.Close()
}

func writeHeader(tw *tar.Writer, name string, size, mode, mtime int64) error {
	return tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     size,
		Mode:     mode,
		Uid:      0,
		Gid:      0,
		Uname:    "root",
		Gname:    "wheel",
		ModTime:  time.Unix(mtime, 0),
		Format:   tar.FormatUSTAR,
	})
}

// copyFile streams src into the archive, insisting on exactly size bytes: a
// file that changed underneath the walk would otherwise corrupt the tar.
func copyFile(tw *tar.Writer, src string, size int64) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(tw, f)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s: read %d bytes, expected %d", src, n, size)
	}
	return nil
}
