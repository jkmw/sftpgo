// Copyright (C) 2019 Nicola Murino
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package sftpd

import (
	"io"
	"net"
	"os"
	"path"
	"time"

	"github.com/pkg/sftp"
	"github.com/sftpgo/sdk"

	"github.com/drakkan/sftpgo/v2/internal/common"
	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/util"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
)

// Connection details for an authenticated user
type Connection struct {
	*common.BaseConnection
	// client's version string
	ClientVersion string
	// Remote address for this connection
	RemoteAddr   net.Addr
	LocalAddr    net.Addr
	channel      io.ReadWriteCloser
	command      string
	folderPrefix string
}

// GetClientVersion returns the connected client's version
func (c *Connection) GetClientVersion() string {
	return c.ClientVersion
}

// GetLocalAddress returns local connection address
func (c *Connection) GetLocalAddress() string {
	if c.LocalAddr == nil {
		return ""
	}
	return c.LocalAddr.String()
}

// GetRemoteAddress returns the connected client's address
func (c *Connection) GetRemoteAddress() string {
	if c.RemoteAddr == nil {
		return ""
	}
	return c.RemoteAddr.String()
}

// GetCommand returns the SSH command, if any
func (c *Connection) GetCommand() string {
	return c.command
}

// Fileread creates a reader for a file on the system and returns the reader back.
func (c *Connection) Fileread(request *sftp.Request) (io.ReaderAt, error) {
	c.UpdateLastActivity()

	if !c.User.HasPerm(dataprovider.PermDownload, path.Dir(request.Filepath)) {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	transferQuota := c.GetTransferQuota()
	if !transferQuota.HasDownloadSpace() {
		c.Log(logger.LevelInfo, "denying file read due to quota limits")
		return nil, c.GetReadQuotaExceededError()
	}

	if ok, policy := c.User.IsFileAllowed(request.Filepath); !ok {
		c.Log(logger.LevelWarn, "reading file %q is not allowed", request.Filepath)
		return nil, c.GetErrorForDeniedFile(policy)
	}

	fs, p, err := c.GetFsAndResolvedPath(request.Filepath)
	if err != nil {
		return nil, err
	}

	if _, err := common.ExecutePreAction(c.BaseConnection, common.OperationPreDownload, p, request.Filepath, 0, 0); err != nil {
		c.Log(logger.LevelDebug, "download for file %q denied by pre action: %v", request.Filepath, err)
		return nil, c.GetPermissionDeniedError()
	}

	file, r, cancelFn, err := fs.Open(p, 0)
	if err != nil {
		c.Log(logger.LevelError, "could not open file %q for reading: %+v", p, err)
		return nil, c.GetFsError(fs, err)
	}

	baseTransfer := common.NewBaseTransfer(file, c.BaseConnection, cancelFn, p, p, request.Filepath, common.TransferDownload,
		0, 0, 0, 0, false, fs, transferQuota)
	t := newTransfer(baseTransfer, nil, r, nil)

	return t, nil
}

// OpenFile implements OpenFileWriter interface
func (c *Connection) OpenFile(request *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return c.handleFilewrite(request)
}

// Filewrite handles the write actions for a file on the system.
func (c *Connection) Filewrite(request *sftp.Request) (io.WriterAt, error) {
	return c.handleFilewrite(request)
}

func (c *Connection) handleFilewrite(request *sftp.Request) (sftp.WriterAtReaderAt, error) {
	c.UpdateLastActivity()

	if ok, _ := c.User.IsFileAllowed(request.Filepath); !ok {
		c.Log(logger.LevelWarn, "writing file %q is not allowed", request.Filepath)
		return nil, c.GetPermissionDeniedError()
	}

	fs, p, err := c.GetFsAndResolvedPath(request.Filepath)
	if err != nil {
		return nil, err
	}

	filePath := p
	if common.Config.IsAtomicUploadEnabled() && fs.IsAtomicUploadSupported() {
		filePath = fs.GetAtomicUploadPath(p)
	}

	var errForRead error
	if !vfs.HasOpenRWSupport(fs) && request.Pflags().Read {
		// read and write mode is only supported for local filesystem
		errForRead = sftp.ErrSSHFxOpUnsupported
	}
	if !c.User.HasPerm(dataprovider.PermDownload, path.Dir(request.Filepath)) {
		// we can try to read only for local fs here, see above.
		// os.ErrPermission will become sftp.ErrSSHFxPermissionDenied when sent to
		// the client
		errForRead = os.ErrPermission
	}

	stat, statErr := fs.Lstat(p)
	if (statErr == nil && stat.Mode()&os.ModeSymlink != 0) || fs.IsNotExist(statErr) {
		if !c.User.HasPerm(dataprovider.PermUpload, path.Dir(request.Filepath)) {
			return nil, sftp.ErrSSHFxPermissionDenied
		}
		return c.handleSFTPUploadToNewFile(fs, request.Pflags(), p, filePath, request.Filepath, errForRead)
	}

	if statErr != nil {
		c.Log(logger.LevelError, "error performing file stat %q: %+v", p, statErr)
		return nil, c.GetFsError(fs, statErr)
	}

	// This happen if we upload a file that has the same name of an existing directory
	if stat.IsDir() {
		c.Log(logger.LevelError, "attempted to open a directory for writing to: %q", p)
		return nil, sftp.ErrSSHFxOpUnsupported
	}

	if !c.User.HasPerm(dataprovider.PermOverwrite, path.Dir(request.Filepath)) {
		return nil, sftp.ErrSSHFxPermissionDenied
	}

	return c.handleSFTPUploadToExistingFile(fs, request.Pflags(), p, filePath, stat.Size(), request.Filepath, errForRead)
}

// Filecmd hander for basic SFTP system calls related to files, but not anything to do with reading
// or writing to those files.
func (c *Connection) Filecmd(request *sftp.Request) error {
	c.UpdateLastActivity()

	switch request.Method {
	case "Setstat":
		return c.handleSFTPSetstat(request)
	case "Rename":
		if err := c.Rename(request.Filepath, request.Target); err != nil {
			return err
		}
	case "Rmdir":
		return c.RemoveDir(request.Filepath)
	case "Mkdir":
		err := c.CreateDir(request.Filepath, true)
		if err != nil {
			return err
		}
	case "Symlink":
		if err := c.CreateSymlink(request.Filepath, request.Target); err != nil {
			return err
		}
	case "Remove":
		return c.handleSFTPRemove(request)
	default:
		return sftp.ErrSSHFxOpUnsupported
	}

	return sftp.ErrSSHFxOk
}

// Filelist is the handler for SFTP filesystem list calls. This will handle calls to list the contents of
// a directory as well as perform file/folder stat calls.
func (c *Connection) Filelist(request *sftp.Request) (sftp.ListerAt, error) {
	c.UpdateLastActivity()

	switch request.Method {
	case "List":
		files, err := c.ListDir(request.Filepath)
		if err != nil {
			return nil, err
		}
		modTime := time.Unix(0, 0)
		if request.Filepath != "/" || c.folderPrefix != "" {
			files = util.PrependFileInfo(files, vfs.NewFileInfo("..", true, 0, modTime, false))
		}
		files = util.PrependFileInfo(files, vfs.NewFileInfo(".", true, 0, modTime, false))
		return listerAt(files), nil
	case "Stat":
		if !c.User.HasPerm(dataprovider.PermListItems, path.Dir(request.Filepath)) {
			return nil, sftp.ErrSSHFxPermissionDenied
		}

		s, err := c.DoStat(request.Filepath, 0, true)
		if err != nil {
			return nil, err
		}

		return listerAt([]os.FileInfo{s}), nil
	default:
		return nil, sftp.ErrSSHFxOpUnsupported
	}
}

// Readlink implements the ReadlinkFileLister interface
func (c *Connection) Readlink(filePath string) (string, error) {
	if err := c.canReadLink(filePath); err != nil {
		return "", err
	}

	fs, p, err := c.GetFsAndResolvedPath(filePath)
	if err != nil {
		return "", err
	}

	s, err := fs.Readlink(p)
	if err != nil {
		c.Log(logger.LevelDebug, "error running readlink on path %q: %+v", p, err)
		return "", c.GetFsError(fs, err)
	}

	if err := c.canReadLink(s); err != nil {
		return "", err
	}
	return s, nil
}

// Lstat implements LstatFileLister interface
func (c *Connection) Lstat(request *sftp.Request) (sftp.ListerAt, error) {
	c.UpdateLastActivity()

	if !c.User.HasPerm(dataprovider.PermListItems, path.Dir(request.Filepath)) {
		return nil, sftp.ErrSSHFxPermissionDenied
	}

	s, err := c.DoStat(request.Filepath, 1, true)
	if err != nil {
		return nil, err
	}

	return listerAt([]os.FileInfo{s}), nil
}

// RealPath implements the RealPathFileLister interface
func (c *Connection) RealPath(p string) (string, error) {
	if !c.User.HasPerm(dataprovider.PermListItems, path.Dir(p)) {
		return "", sftp.ErrSSHFxPermissionDenied
	}

	if c.User.Filters.StartDirectory == "" {
		p = util.CleanPath(p)
	} else {
		p = util.CleanPathWithBase(c.User.Filters.StartDirectory, p)
	}
	fs, fsPath, err := c.GetFsAndResolvedPath(p)
	if err != nil {
		return "", err
	}
	if realPather, ok := fs.(vfs.FsRealPather); ok {
		realPath, err := realPather.RealPath(fsPath)
		if err != nil {
			return "", c.GetFsError(fs, err)
		}
		return realPath, nil
	}
	return p, nil
}

// StatVFS implements StatVFSFileCmder interface
func (c *Connection) StatVFS(r *sftp.Request) (*sftp.StatVFS, error) {
	c.UpdateLastActivity()

	// we are assuming that r.Filepath is a dir, this could be wrong but should
	// not produce any side effect here.
	// we don't consider c.User.Filters.MaxUploadFileSize, we return disk stats here
	// not the limit for a single file upload
	quotaResult, _ := c.HasSpace(true, true, path.Join(r.Filepath, "fakefile.txt"))

	fs, p, err := c.GetFsAndResolvedPath(r.Filepath)
	if err != nil {
		return nil, err
	}

	if !quotaResult.HasSpace {
		return c.getStatVFSFromQuotaResult(fs, p, quotaResult)
	}

	if quotaResult.QuotaSize == 0 && quotaResult.QuotaFiles == 0 {
		// no quota restrictions
		statvfs, err := fs.GetAvailableDiskSize(p)
		if err == vfs.ErrStorageSizeUnavailable {
			return c.getStatVFSFromQuotaResult(fs, p, quotaResult)
		}
		return statvfs, err
	}

	// there is free space but some limits are configured
	return c.getStatVFSFromQuotaResult(fs, p, quotaResult)
}

func (c *Connection) canReadLink(name string) error {
	if !c.User.HasPerm(dataprovider.PermListItems, path.Dir(name)) {
		return sftp.ErrSSHFxPermissionDenied
	}
	ok, policy := c.User.IsFileAllowed(name)
	if !ok && policy == sdk.DenyPolicyHide {
		return sftp.ErrSSHFxNoSuchFile
	}
	return nil
}

func (c *Connection) handleSFTPSetstat(request *sftp.Request) error {
	attrs := common.StatAttributes{
		Flags: 0,
	}
	if request.AttrFlags().Permissions {
		attrs.Flags |= common.StatAttrPerms
		attrs.Mode = request.Attributes().FileMode()
	}
	if request.AttrFlags().UidGid {
		attrs.Flags |= common.StatAttrUIDGID
		attrs.UID = int(request.Attributes().UID)
		attrs.GID = int(request.Attributes().GID)
	}
	if request.AttrFlags().Acmodtime {
		attrs.Flags |= common.StatAttrTimes
		attrs.Atime = time.Unix(int64(request.Attributes().Atime), 0)
		attrs.Mtime = time.Unix(int64(request.Attributes().Mtime), 0)
	}
	if request.AttrFlags().Size {
		attrs.Flags |= common.StatAttrSize
		attrs.Size = int64(request.Attributes().Size)
	}

	return c.SetStat(request.Filepath, &attrs)
}

func (c *Connection) handleSFTPRemove(request *sftp.Request) error {
	fs, fsPath, err := c.GetFsAndResolvedPath(request.Filepath)
	if err != nil {
		return err
	}

	var fi os.FileInfo
	if fi, err = fs.Lstat(fsPath); err != nil {
		c.Log(logger.LevelDebug, "failed to remove file %q: stat error: %+v", fsPath, err)
		return c.GetFsError(fs, err)
	}
	if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		c.Log(logger.LevelDebug, "cannot remove %q is not a file/symlink", fsPath)
		return sftp.ErrSSHFxFailure
	}

	return c.RemoveFile(fs, fsPath, request.Filepath, fi)
}

func (c *Connection) handleSFTPUploadToNewFile(fs vfs.Fs, pflags sftp.FileOpenFlags, resolvedPath, filePath, requestPath string, errForRead error) (sftp.WriterAtReaderAt, error) {
	diskQuota, transferQuota := c.HasSpace(true, false, requestPath)
	if !diskQuota.HasSpace || !transferQuota.HasUploadSpace() {
		c.Log(logger.LevelInfo, "denying file write due to quota limits")
		return nil, c.GetQuotaExceededError()
	}

	if _, err := common.ExecutePreAction(c.BaseConnection, common.OperationPreUpload, resolvedPath, requestPath, 0, 0); err != nil {
		c.Log(logger.LevelDebug, "upload for file %q denied by pre action: %v", requestPath, err)
		return nil, c.GetPermissionDeniedError()
	}

	osFlags := getOSOpenFlags(pflags)
	file, w, cancelFn, err := fs.Create(filePath, osFlags, c.GetCreateChecks(requestPath, true, false))
	if err != nil {
		c.Log(logger.LevelError, "error creating file %q, os flags %d, pflags %+v: %+v", resolvedPath, osFlags, pflags, err)
		return nil, c.GetFsError(fs, err)
	}

	vfs.SetPathPermissions(fs, filePath, c.User.GetUID(), c.User.GetGID())

	// we can get an error only for resume
	maxWriteSize, _ := c.GetMaxWriteSize(diskQuota, false, 0, fs.IsUploadResumeSupported())

	baseTransfer := common.NewBaseTransfer(file, c.BaseConnection, cancelFn, resolvedPath, filePath, requestPath,
		common.TransferUpload, 0, 0, maxWriteSize, 0, true, fs, transferQuota)
	t := newTransfer(baseTransfer, w, nil, errForRead)

	return t, nil
}

func (c *Connection) handleSFTPUploadToExistingFile(fs vfs.Fs, pflags sftp.FileOpenFlags, resolvedPath, filePath string,
	fileSize int64, requestPath string, errForRead error) (sftp.WriterAtReaderAt, error) {
	var err error
	diskQuota, transferQuota := c.HasSpace(false, false, requestPath)
	if !diskQuota.HasSpace || !transferQuota.HasUploadSpace() {
		c.Log(logger.LevelInfo, "denying file write due to quota limits")
		return nil, c.GetQuotaExceededError()
	}

	osFlags := getOSOpenFlags(pflags)
	minWriteOffset := int64(0)
	isTruncate := osFlags&os.O_TRUNC != 0
	// for upload resumes OpenSSH sets the APPEND flag while WinSCP does not set it,
	// so we suppose this is an upload resume if the TRUNCATE flag is not set
	isResume := !isTruncate
	// if there is a size limit the remaining size cannot be 0 here, since quotaResult.HasSpace
	// will return false in this case and we deny the upload before.
	// For Cloud FS GetMaxWriteSize will return unsupported operation
	maxWriteSize, err := c.GetMaxWriteSize(diskQuota, isResume, fileSize, vfs.IsUploadResumeSupported(fs, fileSize))
	if err != nil {
		c.Log(logger.LevelDebug, "unable to get max write size for file %q is resume? %t: %v",
			requestPath, isResume, err)
		return nil, err
	}

	if _, err := common.ExecutePreAction(c.BaseConnection, common.OperationPreUpload, resolvedPath, requestPath, fileSize, osFlags); err != nil {
		c.Log(logger.LevelDebug, "upload for file %q denied by pre action: %v", requestPath, err)
		return nil, c.GetPermissionDeniedError()
	}

	if common.Config.IsAtomicUploadEnabled() && fs.IsAtomicUploadSupported() {
		_, _, err = fs.Rename(resolvedPath, filePath)
		if err != nil {
			c.Log(logger.LevelError, "error renaming existing file for atomic upload, source: %q, dest: %q, err: %+v",
				resolvedPath, filePath, err)
			return nil, c.GetFsError(fs, err)
		}
	}

	file, w, cancelFn, err := fs.Create(filePath, osFlags, c.GetCreateChecks(requestPath, false, isResume))
	if err != nil {
		c.Log(logger.LevelError, "error opening existing file, os flags %v, pflags: %+v, source: %q, err: %+v",
			osFlags, pflags, filePath, err)
		return nil, c.GetFsError(fs, err)
	}

	initialSize := int64(0)
	truncatedSize := int64(0) // bytes truncated and not included in quota
	if isResume {
		c.Log(logger.LevelDebug, "resuming upload requested, file path %q initial size: %d, has append flag %t",
			filePath, fileSize, pflags.Append)
		// enforce min write offset only if the client passed the APPEND flag or the filesystem
		// supports emulated resume
		if pflags.Append || !fs.IsUploadResumeSupported() {
			minWriteOffset = fileSize
		}
		initialSize = fileSize
	} else {
		if isTruncate && vfs.HasTruncateSupport(fs) {
			c.updateQuotaAfterTruncate(requestPath, fileSize)
		} else {
			initialSize = fileSize
			truncatedSize = fileSize
		}
	}

	vfs.SetPathPermissions(fs, filePath, c.User.GetUID(), c.User.GetGID())

	baseTransfer := common.NewBaseTransfer(file, c.BaseConnection, cancelFn, resolvedPath, filePath, requestPath,
		common.TransferUpload, minWriteOffset, initialSize, maxWriteSize, truncatedSize, false, fs, transferQuota)
	t := newTransfer(baseTransfer, w, nil, errForRead)

	return t, nil
}

// Disconnect disconnects the client by closing the channel
func (c *Connection) Disconnect() error {
	if c.channel == nil {
		c.Log(logger.LevelWarn, "cannot disconnect a nil channel")
		return nil
	}
	return c.channel.Close()
}

func (c *Connection) getStatVFSFromQuotaResult(fs vfs.Fs, name string, quotaResult vfs.QuotaCheckResult) (*sftp.StatVFS, error) {
	s, err := fs.GetAvailableDiskSize(name)
	if err == nil {
		if quotaResult.QuotaSize == 0 || quotaResult.QuotaSize > int64(s.TotalSpace()) {
			quotaResult.QuotaSize = int64(s.TotalSpace())
		}
		if quotaResult.QuotaFiles == 0 || quotaResult.QuotaFiles > int(s.Files) {
			quotaResult.QuotaFiles = int(s.Files)
		}
	} else if err != vfs.ErrStorageSizeUnavailable {
		return nil, err
	}
	// if we are unable to get quota size or quota files we add some arbitrary values
	if quotaResult.QuotaSize == 0 {
		quotaResult.QuotaSize = quotaResult.UsedSize + 8*1024*1024*1024*1024 // 8TB
	}
	if quotaResult.QuotaFiles == 0 {
		quotaResult.QuotaFiles = quotaResult.UsedFiles + 1000000 // 1 million
	}

	bsize := uint64(4096)
	for bsize > uint64(quotaResult.QuotaSize) {
		bsize /= 4
	}
	blocks := uint64(quotaResult.QuotaSize) / bsize
	bfree := uint64(quotaResult.QuotaSize-quotaResult.UsedSize) / bsize
	files := uint64(quotaResult.QuotaFiles)
	ffree := uint64(quotaResult.QuotaFiles - quotaResult.UsedFiles)
	if !quotaResult.HasSpace {
		bfree = 0
		ffree = 0
	}

	return &sftp.StatVFS{
		Bsize:   bsize,
		Frsize:  bsize,
		Blocks:  blocks,
		Bfree:   bfree,
		Bavail:  bfree,
		Files:   files,
		Ffree:   ffree,
		Favail:  ffree,
		Namemax: 255,
	}, nil
}

func (c *Connection) updateQuotaAfterTruncate(requestPath string, fileSize int64) {
	vfolder, err := c.User.GetVirtualFolderForPath(path.Dir(requestPath))
	if err == nil {
		dataprovider.UpdateVirtualFolderQuota(&vfolder.BaseVirtualFolder, 0, -fileSize, false) //nolint:errcheck
		if vfolder.IsIncludedInUserQuota() {
			dataprovider.UpdateUserQuota(&c.User, 0, -fileSize, false) //nolint:errcheck
		}
	} else {
		dataprovider.UpdateUserQuota(&c.User, 0, -fileSize, false) //nolint:errcheck
	}
}

func getOSOpenFlags(requestFlags sftp.FileOpenFlags) (flags int) {
	var osFlags int
	if requestFlags.Read && requestFlags.Write {
		osFlags |= os.O_RDWR
	} else if requestFlags.Write {
		osFlags |= os.O_WRONLY
	}
	// we ignore Append flag since pkg/sftp use WriteAt that cannot work with os.O_APPEND
	/*if requestFlags.Append {
		osFlags |= os.O_APPEND
	}*/
	if requestFlags.Creat {
		osFlags |= os.O_CREATE
	}
	if requestFlags.Trunc {
		osFlags |= os.O_TRUNC
	}
	if requestFlags.Excl {
		osFlags |= os.O_EXCL
	}
	return osFlags
}
