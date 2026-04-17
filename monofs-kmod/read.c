// SPDX-License-Identifier: GPL-2.0-only

#include <linux/fs.h>
#include <linux/mm.h>
#include <linux/pagemap.h>
#include <linux/string.h>
#include <linux/uio.h>

#include "monofs.h"

static int monofs_refresh_remote_inode(struct inode *inode, bool invalidate_pages)
{
	struct monofs_inode_ctx *ctx = monofs_inode_ctx(inode);
	struct monofs_fs_info *fsi = inode->i_sb->s_fs_info;
	u64 generation;
	int ret;

	if (!ctx || !ctx->has_object_id || !monofs_native_connected(fsi))
		return 0;

	ret = monofs_native_ping(fsi, &generation);
	if (ret)
		return ret;
	if (ctx->generation == generation)
		return 0;

	mutex_lock(&ctx->read_handle_lock);
	if (ctx->has_read_handle && monofs_native_connected(fsi))
		monofs_native_close(fsi, ctx->read_handle);
	ctx->has_read_handle = false;
	ctx->read_handle = 0;
	ctx->generation = generation;
	mutex_unlock(&ctx->read_handle_lock);

	if (invalidate_pages)
		invalidate_remote_inode(inode);
	return 0;
}

static int monofs_read_remote_page(struct inode *inode, loff_t offset, size_t len,
				   void *data, u32 *bytes_read, bool *eof)
{
	struct monofs_inode_ctx *ctx = monofs_inode_ctx(inode);
	struct monofs_fs_info *fsi = inode->i_sb->s_fs_info;
	struct monofs_native_attr attr;
	int ret = 0;

	if (!ctx || !ctx->has_object_id || !monofs_native_connected(fsi))
		return -EIO;

	mutex_lock(&ctx->read_handle_lock);
	if (!ctx->has_read_handle) {
		ret = monofs_native_open(fsi, ctx->object_id, &ctx->read_handle, &attr);
		if (!ret) {
			ctx->has_read_handle = true;
			ctx->generation = fsi->native_namespace_generation;
			monofs_apply_native_attr(inode, &attr);
		}
	}
	if (!ret)
		ret = monofs_native_read(fsi, ctx->read_handle, offset, len,
					 data, bytes_read, eof);
	mutex_unlock(&ctx->read_handle_lock);
	return ret;
}

static int monofs_read_folio(struct file *file, struct folio *folio)
{
	struct inode *inode = folio->mapping->host;
	size_t len = folio_size(folio);
	loff_t offset = ((loff_t)folio->index) << PAGE_SHIFT;
	u8 *kaddr;
	u32 bytes_read = 0;
	bool eof = false;
	int ret;

	(void)file;

	ret = monofs_refresh_remote_inode(inode, false);
	if (ret)
		goto error;

	kaddr = kmap_local_folio(folio, 0);
	ret = monofs_read_remote_page(inode, offset, len, kaddr, &bytes_read, &eof);
	if (!ret && bytes_read < len)
		memset(kaddr + bytes_read, 0, len - bytes_read);
	kunmap_local(kaddr);

	if (ret)
		goto error;

	flush_dcache_folio(folio);
	folio_mark_uptodate(folio);
	folio_unlock(folio);
	return 0;

error:
	folio_set_error(folio);
	folio_unlock(folio);
	return ret;
}

const struct address_space_operations monofs_remote_aops = {
	.read_folio = monofs_read_folio,
};

static int monofs_remote_open(struct inode *inode, struct file *file)
{
	int ret = monofs_refresh_remote_inode(inode, true);

	if (ret)
		return ret;
	return generic_file_open(inode, file);
}

static ssize_t monofs_remote_read_iter(struct kiocb *iocb, struct iov_iter *to)
{
	int ret = monofs_refresh_remote_inode(file_inode(iocb->ki_filp), true);

	if (ret)
		return ret;
	return generic_file_read_iter(iocb, to);
}

static int monofs_remote_mmap(struct file *file, struct vm_area_struct *vma)
{
	int ret = monofs_refresh_remote_inode(file_inode(file), true);

	if (ret)
		return ret;
	return generic_file_mmap(file, vma);
}

const struct file_operations monofs_remote_file_operations = {
	.open = monofs_remote_open,
	.read_iter = monofs_remote_read_iter,
	.llseek = generic_file_llseek,
	.mmap = monofs_remote_mmap,
	.splice_read = filemap_splice_read,
};
