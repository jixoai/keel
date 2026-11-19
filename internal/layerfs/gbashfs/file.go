package gbashfs

import (
	stdfs "io/fs"
	"os"
	"path"
	"syscall"
	"time"
)

// fileInfo is the stdfs.FileInfo view of a node.
type fileInfo struct {
	name string
	node *node
}

func infoFor(name string, n *node) fileInfo {
	return fileInfo{name: name, node: n}
}

func (i fileInfo) Name() string { return i.name }

func (i fileInfo) Size() int64 {
	switch i.node.kind {
	case nodeFile:
		return i.node.size
	case nodeSymlink:
		return int64(len(i.node.target))
	default:
		return 0
	}
}

func (i fileInfo) Mode() stdfs.FileMode {
	switch i.node.kind {
	case nodeDir:
		return stdfs.ModeDir | i.node.mode
	case nodeSymlink:
		return stdfs.ModeSymlink | i.node.mode
	default:
		return i.node.mode
	}
}

func (i fileInfo) ModTime() time.Time { return i.node.mtime }

func (i fileInfo) IsDir() bool { return i.node.kind == nodeDir }

func (i fileInfo) Sys() any { return nil }

type dirEntry struct {
	name string
	info fileInfo
}

func (e dirEntry) Name() string                  { return e.name }
func (e dirEntry) IsDir() bool                   { return e.info.IsDir() }
func (e dirEntry) Type() stdfs.FileMode          { return e.info.Mode().Type() }
func (e dirEntry) Info() (stdfs.FileInfo, error) { return e.info, nil }

// fileHandle is one open file. Reads stream from the CAS object; writes spool
// to a temp file and enter the CAS on Close, so a large file never sits in
// memory.
type fileHandle struct {
	ws      *Workspace
	path    string
	file    *os.File
	tmpPath string
	flag    int
	write   bool
	mode    stdfs.FileMode
	size    int64
	closed  bool
}

func (h *fileHandle) Read(p []byte) (int, error) { return h.file.Read(p) }

func (h *fileHandle) Write(p []byte) (int, error) {
	if !h.write {
		return 0, &os.PathError{Op: "write", Path: h.path, Err: syscall.EBADF}
	}
	n, err := h.file.Write(p)
	h.size += int64(n)
	return n, err
}

func (h *fileHandle) Seek(offset int64, whence int) (int64, error) {
	return h.file.Seek(offset, whence)
}

func (h *fileHandle) Close() error {
	if h.closed {
		return nil
	}
	h.closed = true
	closeErr := h.file.Close()
	if !h.write {
		if h.tmpPath != "" {
			_ = os.Remove(h.tmpPath)
		}
		return closeErr
	}
	defer os.Remove(h.tmpPath)
	if closeErr != nil {
		return closeErr
	}
	result, err := h.ws.store.PutFile(h.tmpPath)
	if err != nil {
		return err
	}
	h.ws.mu.Lock()
	defer h.ws.mu.Unlock()
	if n := h.ws.nodes[h.path]; n != nil && n.kind == nodeFile {
		n.hash = result.Hash
		n.size = result.Size
		n.mtime = h.ws.now()
		n.mode = h.mode
	}
	return nil
}

func (h *fileHandle) Stat() (stdfs.FileInfo, error) {
	return infoFor(path.Base(h.path), &node{
		kind:  nodeFile,
		mode:  h.mode,
		size:  h.size,
		mtime: h.ws.now(),
	}), nil
}
