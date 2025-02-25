package fs

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
	"testing/iotest"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/wasi"
)

var testCtx = context.Background()

//go:embed testdata/animals.txt
var animals []byte

// wasiFs is an implementation of fs.Fs calling into wasi. Not thread-safe because we use
// fixed Memory offsets for transferring data with wasm.
type wasiFs struct {
	t *testing.T

	wasm   wazero.Runtime
	memory api.Memory

	workdirFd uint32

	pathOpen api.Function
	fdClose  api.Function
	fdRead   api.Function
	fdSeek   api.Function
}

func (fs *wasiFs) Open(name string) (fs.File, error) {
	pathBytes := []byte(name)
	// Pick anywhere in memory to write the path to.
	pathPtr := uint32(0)
	ok := fs.memory.Write(testCtx, pathPtr, pathBytes)
	require.True(fs.t, ok)
	resultOpenedFd := pathPtr + uint32(len(pathBytes))

	fd := fs.workdirFd
	dirflags := uint32(0) // arbitrary dirflags
	pathLen := len(pathBytes)
	oflags := uint32(0) // arbitrary oflags
	// rights are ignored per https://github.com/WebAssembly/WASI/issues/469#issuecomment-1045251844
	fsRightsBase, fsRightsInheriting := uint64(1), uint64(2)
	fdflags := uint32(0) // arbitrary fdflags
	res, err := fs.pathOpen.Call(
		testCtx,
		uint64(fd), uint64(dirflags), uint64(pathPtr), uint64(pathLen), uint64(oflags),
		fsRightsBase, fsRightsInheriting, uint64(fdflags), uint64(resultOpenedFd))
	require.NoError(fs.t, err)
	require.Equal(fs.t, uint64(wasi.ErrnoSuccess), res[0])

	resFd, ok := fs.memory.ReadUint32Le(testCtx, resultOpenedFd)
	require.True(fs.t, ok)

	return &wasiFile{fd: resFd, fs: fs}, nil
}

// wasiFile implements io.Reader and io.Seeker using wasi functions. It does not
// implement io.ReaderAt because there is no wasi function for directly reading
// from an offset.
type wasiFile struct {
	fd uint32
	fs *wasiFs
}

func (f *wasiFile) Stat() (fs.FileInfo, error) {
	// We currently don't implement wasi's fd_stat but also don't use this method from this test.
	panic("unused")
}

func (f *wasiFile) Read(bytes []byte) (int, error) {
	// Pick anywhere in memory for wasm to write resultSize too. We do this first since it's fixed length
	// while iovs is variable.
	resultSizeOff := uint32(0)
	// Next place iovs
	iovsOff := uint32(4)
	// We do not directly write to hardware, there is no need for more than one iovec
	iovsCount := uint32(1)
	// iov starts at iovsOff + 8 because we first write four bytes for the offset itself, and
	// four bytes for the length of the iov.
	iovOff := iovsOff + uint32(8)
	ok := f.fs.memory.WriteUint32Le(testCtx, iovsOff, iovOff)
	require.True(f.fs.t, ok)
	// Next write the length.
	ok = f.fs.memory.WriteUint32Le(testCtx, iovsOff+uint32(4), uint32(len(bytes)))
	require.True(f.fs.t, ok)

	res, err := f.fs.fdRead.Call(testCtx, uint64(f.fd), uint64(iovsOff), uint64(iovsCount), uint64(resultSizeOff))
	require.NoError(f.fs.t, err)

	require.NotEqual(f.fs.t, uint64(wasi.ErrnoFault), res[0])

	numRead, ok := f.fs.memory.ReadUint32Le(testCtx, resultSizeOff)
	require.True(f.fs.t, ok)

	if numRead == 0 {
		if len(bytes) == 0 {
			return 0, nil
		}
		if wasi.Errno(res[0]) == wasi.ErrnoSuccess {
			return 0, io.EOF
		} else {
			return 0, fmt.Errorf("could not read from file")
		}
	}

	buf, ok := f.fs.memory.Read(testCtx, iovOff, numRead)
	require.True(f.fs.t, ok)
	copy(bytes, buf)
	return int(numRead), nil
}

func (f *wasiFile) Close() error {
	res, err := f.fs.fdClose.Call(testCtx, uint64(f.fd))
	require.NoError(f.fs.t, err)
	require.NotEqual(f.fs.t, uint64(wasi.ErrnoFault), res[0])
	return nil
}

func (f *wasiFile) Seek(offset int64, whence int) (int64, error) {
	// Pick anywhere in memory for wasm to write the result newOffset to
	resultNewoffsetOff := uint32(0)

	res, err := f.fs.fdSeek.Call(testCtx, uint64(f.fd), uint64(offset), uint64(whence), uint64(resultNewoffsetOff))
	require.NoError(f.fs.t, err)
	require.NotEqual(f.fs.t, uint64(wasi.ErrnoFault), res[0])

	newOffset, ok := f.fs.memory.ReadUint32Le(testCtx, resultNewoffsetOff)
	require.True(f.fs.t, ok)

	return int64(newOffset), nil
}

func TestReader(t *testing.T) {
	r := wazero.NewRuntime()
	defer r.Close(testCtx)

	_, err := wasi.InstantiateSnapshotPreview1(testCtx, r)
	require.NoError(t, err)

	realFs := fstest.MapFS{"animals.txt": &fstest.MapFile{Data: animals}}
	sys := wazero.NewModuleConfig().WithWorkDirFS(realFs)

	// Create a module that just delegates to wasi functions.
	compiled, err := r.CompileModule(testCtx, []byte(`(module
  (import "wasi_snapshot_preview1" "path_open"
    (func $wasi.path_open (param $fd i32) (param $dirflags i32) (param $path i32) (param $path_len i32) (param $oflags i32) (param $fs_rights_base i64) (param $fs_rights_inheriting i64) (param $fdflags i32) (param $result.opened_fd i32) (result (;errno;) i32)))
  (import "wasi_snapshot_preview1" "fd_close"
    (func $wasi.fd_close (param $fd i32) (result (;errno;) i32)))
  (import "wasi_snapshot_preview1" "fd_read"
    (func $wasi.fd_read (param $fd i32) (param $iovs i32) (param $iovs_len i32) (param $result.size i32) (result (;errno;) i32)))
  (import "wasi_snapshot_preview1" "fd_seek"
    (func $wasi.fd_seek (param $fd i32) (param $offset i64) (param $whence i32) (param $result.newoffset i32) (result (;errno;) i32)))
  (memory 1 1)  ;; just an arbitrary size big enough for tests
  (export "memory" (memory 0))
  (export "path_open" (func $wasi.path_open))
  (export "fd_close" (func $wasi.fd_close))
  (export "fd_read" (func $wasi.fd_read))
  (export "fd_seek" (func $wasi.fd_seek))
)`), wazero.NewCompileConfig())
	require.NoError(t, err)

	mod, err := r.InstantiateModule(testCtx, compiled, sys)
	require.NoError(t, err)

	pathOpen := mod.ExportedFunction("path_open")
	fdClose := mod.ExportedFunction("fd_close")
	fdRead := mod.ExportedFunction("fd_read")
	fdSeek := mod.ExportedFunction("fd_seek")

	wasiFs := &wasiFs{
		t:         t,
		wasm:      r,
		memory:    mod.Memory(),
		workdirFd: uint32(3),
		pathOpen:  pathOpen,
		fdClose:   fdClose,
		fdRead:    fdRead,
		fdSeek:    fdSeek,
	}

	f, err := wasiFs.Open("animals.txt")
	require.NoError(t, err)
	defer f.Close()

	err = iotest.TestReader(f, animals)
	require.NoError(t, err)
}
