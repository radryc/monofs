// SPDX-License-Identifier: GPL-2.0-only

#include <linux/fs.h>
#include <linux/kernel.h>
#include <linux/pagemap.h>
#include <linux/seq_file.h>
#include <linux/slab.h>
#include <linux/statfs.h>
#include <linux/dcache.h>

#include "monofs.h"

static void monofs_evict_inode(struct inode *inode);
static int monofs_statfs(struct dentry *dentry, struct kstatfs *buf);

static int monofs_show_options(struct seq_file *m, struct dentry *root)
{
	struct monofs_fs_info *fsi = root->d_sb->s_fs_info;
	u32 i;

	if (fsi->mount_opts.gateway)
		seq_show_option(m, "gateway", fsi->mount_opts.gateway);
	if (fsi->mount_opts.attr_ttl_ms != MONOFS_DEFAULT_ATTR_TTL_MS)
		seq_printf(m, ",attr_ttl_ms=%u", fsi->mount_opts.attr_ttl_ms);
	if (fsi->mount_opts.seed_path_count) {
		seq_puts(m, ",seed_paths=");
		for (i = 0; i < fsi->mount_opts.seed_path_count; i++) {
			if (i)
				seq_putc(m, ',');
			seq_puts(m, fsi->mount_opts.seed_paths[i]);
		}
	}
	if (fsi->mount_opts.overlay_writes)
		seq_puts(m, ",overlay_writes");
	if (fsi->cluster_version)
		seq_printf(m, ",cluster_version=%llu",
			   (unsigned long long)fsi->cluster_version);
	if (fsi->mount_opts.debug)
		seq_puts(m, ",debug");

	return 0;
}

static const struct super_operations monofs_super_ops = {
	.evict_inode	= monofs_evict_inode,
	.statfs		= monofs_statfs,
	.show_options	= monofs_show_options,
};

static void monofs_evict_inode(struct inode *inode)
{
	struct monofs_inode_ctx *ctx = monofs_inode_ctx(inode);
	struct monofs_fs_info *fsi = inode->i_sb->s_fs_info;

	truncate_inode_pages_final(&inode->i_data);
	clear_inode(inode);

	if (!ctx)
		return;

	if (ctx->has_read_handle && monofs_native_connected(fsi))
		monofs_native_close(fsi, ctx->read_handle);
	kfree(ctx->path);
	kfree(ctx);
	inode->i_private = NULL;
}

static int monofs_statfs(struct dentry *dentry, struct kstatfs *buf)
{
	struct monofs_fs_info *fsi = dentry->d_sb->s_fs_info;
	struct monofs_native_statfs_reply reply;
	int ret;

	if (!monofs_native_connected(fsi))
		return simple_statfs(dentry, buf);

	ret = monofs_native_statfs(fsi, &reply);
	if (ret)
		return ret;

	buf->f_type = MONOFS_MAGIC;
	buf->f_bsize = reply.bsize;
	buf->f_frsize = reply.frsize;
	buf->f_blocks = reply.blocks;
	buf->f_bfree = reply.bfree;
	buf->f_bavail = reply.bavail;
	buf->f_files = reply.files;
	buf->f_ffree = reply.ffree;
	buf->f_namelen = reply.name_len;
	return 0;
}

struct inode *monofs_get_inode(struct super_block *sb,
			       const struct inode *dir,
			       umode_t mode,
			       u64 ino,
			       enum monofs_inode_kind kind,
			       const char *path,
			       const u8 *object_id)
{
	struct monofs_inode_ctx *ctx;
	struct inode *inode;
	struct monofs_fs_info *fsi = sb->s_fs_info;

	ctx = kzalloc(sizeof(*ctx), GFP_KERNEL);
	if (!ctx)
		return NULL;

	inode = new_inode(sb);
	if (!inode) {
		kfree(ctx);
		return NULL;
	}

	inode->i_ino = ino ? ino : get_next_ino();
	inode_init_owner(&nop_mnt_idmap, inode, dir, mode);
	simple_inode_init_ts(inode);
	ctx->kind = kind;
	ctx->path = kstrdup(path ? path : "", GFP_KERNEL);
	if (!ctx->path) {
		kfree(ctx);
		iput(inode);
		return NULL;
	}
	if (object_id) {
		memcpy(ctx->object_id, object_id, MONOFS_NATIVE_OBJECT_ID_LEN);
		ctx->has_object_id = true;
	}
	if (fsi)
		ctx->generation = fsi->native_namespace_generation;
	mutex_init(&ctx->read_handle_lock);
	inode->i_private = ctx;

	switch (mode & S_IFMT) {
	case S_IFDIR:
		inode->i_op = &monofs_dir_inode_operations;
		inode->i_fop = &monofs_dir_operations;
		inc_nlink(inode);
		break;
	case S_IFREG:
		if (kind == MONOFS_INODE_STATUS_FILE || kind == MONOFS_INODE_SEEDS_FILE)
			inode->i_fop = &monofs_control_file_operations;
		else {
			inode->i_fop = &monofs_remote_file_operations;
			inode->i_mapping->a_ops = &monofs_remote_aops;
		}
		break;
	default:
		init_special_inode(inode, mode, 0);
		break;
	}

	return inode;
}

void monofs_apply_native_attr(struct inode *inode,
			      const struct monofs_native_attr *attr)
{
	inode->i_ino = attr->ino ? attr->ino : inode->i_ino;
	inode->i_size = attr->size;
	inode->i_mode = attr->mode;
	inode_set_atime(inode, attr->atime, 0);
	inode_set_mtime(inode, attr->mtime, 0);
	inode_set_ctime(inode, attr->ctime, 0);
	set_nlink(inode, attr->nlink ? attr->nlink : 1);
	i_uid_write(inode, attr->uid);
	i_gid_write(inode, attr->gid);
}

void monofs_free_fs_info(struct monofs_fs_info *fsi)
{
	if (!fsi)
		return;

	monofs_native_disconnect(fsi);
	kfree(fsi->mount_opts.gateway);
	fsi->mount_opts.gateway = NULL;
	kfree(fsi->mount_opts.auth_token);
	fsi->mount_opts.auth_token = NULL;
	while (fsi->mount_opts.seed_path_count > 0) {
		u32 idx = fsi->mount_opts.seed_path_count - 1;

		kfree(fsi->mount_opts.seed_paths[idx]);
		fsi->mount_opts.seed_path_count--;
	}
	kfree(fsi->mount_opts.seed_paths);
	fsi->mount_opts.seed_paths = NULL;
}

int monofs_fill_super(struct super_block *sb, struct fs_context *fc)
{
	struct monofs_fs_info *fsi = sb->s_fs_info;
	struct inode *inode;
	int ret;

	(void)fc;

	sb->s_maxbytes = MAX_LFS_FILESIZE;
	sb->s_blocksize = PAGE_SIZE;
	sb->s_blocksize_bits = PAGE_SHIFT;
	sb->s_magic = MONOFS_MAGIC;
	sb->s_op = &monofs_super_ops;
	sb->s_time_gran = 1;

	if (!fsi->mount_opts.attr_ttl_ms)
		fsi->mount_opts.attr_ttl_ms = MONOFS_DEFAULT_ATTR_TTL_MS;

	if (fsi->mount_opts.gateway) {
		ret = monofs_native_mount(fsi);
		if (ret)
			return ret;
	}

	inode = monofs_get_inode(sb, NULL,
				 monofs_native_connected(fsi) ? fsi->native_root_attr.mode : (S_IFDIR | 0555),
				 MONOFS_ROOT_INO, MONOFS_INODE_ROOT, "",
				 monofs_native_connected(fsi) ? fsi->native_root_object_id : NULL);
	if (inode && monofs_native_connected(fsi))
		monofs_apply_native_attr(inode, &fsi->native_root_attr);
	sb->s_root = d_make_root(inode);
	if (!sb->s_root)
		return -ENOMEM;
	d_set_d_op(sb->s_root, &monofs_dentry_operations);
	sb->s_root->d_time = (unsigned long)fsi->native_namespace_generation;

	if (fsi->mount_opts.debug)
		pr_info("mounted scaffold gateway=%s overlay_writes=%u cluster_version=%llu namespace_generation=%llu seed_paths=%u native_connected=%u\n",
			fsi->mount_opts.gateway ? fsi->mount_opts.gateway : "<unset>",
			fsi->mount_opts.overlay_writes,
			(unsigned long long)fsi->cluster_version,
			(unsigned long long)fsi->native_namespace_generation,
			fsi->mount_opts.seed_path_count,
			fsi->native_connected);

	return 0;
}

void monofs_kill_sb(struct super_block *sb)
{
	struct monofs_fs_info *fsi = sb->s_fs_info;

	monofs_free_fs_info(fsi);
	kfree(fsi);
	kill_anon_super(sb);
}
