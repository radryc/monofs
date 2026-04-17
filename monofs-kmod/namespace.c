// SPDX-License-Identifier: GPL-2.0-only

#include <linux/dcache.h>
#include <linux/errno.h>
#include <linux/fs.h>
#include <linux/kernel.h>
#include <linux/namei.h>
#include <linux/slab.h>
#include <linux/string.h>

#include "monofs.h"

struct monofs_synth_entry {
	const char *name;
	u64 ino;
	unsigned int d_type;
	enum monofs_inode_kind kind;
	umode_t mode;
};

static const struct monofs_synth_entry monofs_root_entries[] = {
	{
		.name = ".monofs",
		.ino = MONOFS_CONTROL_INO,
		.d_type = DT_DIR,
		.kind = MONOFS_INODE_CONTROL_DIR,
		.mode = S_IFDIR | 0555,
	},
};

static const struct monofs_synth_entry monofs_control_entries[] = {
	{
		.name = "status",
		.ino = MONOFS_STATUS_INO,
		.d_type = DT_REG,
		.kind = MONOFS_INODE_STATUS_FILE,
		.mode = S_IFREG | 0444,
	},
	{
		.name = "seeds",
		.ino = MONOFS_SEEDS_INO,
		.d_type = DT_REG,
		.kind = MONOFS_INODE_SEEDS_FILE,
		.mode = S_IFREG | 0444,
	},
};

static const struct monofs_synth_entry *
monofs_get_dir_entries(enum monofs_inode_kind kind, size_t *count)
{
	switch (kind) {
	case MONOFS_INODE_ROOT:
		*count = ARRAY_SIZE(monofs_root_entries);
		return monofs_root_entries;
	case MONOFS_INODE_CONTROL_DIR:
		*count = ARRAY_SIZE(monofs_control_entries);
		return monofs_control_entries;
	default:
		*count = 0;
		return NULL;
	}
}

static const struct monofs_synth_entry *
monofs_find_child(enum monofs_inode_kind kind, const struct qstr *name)
{
	const struct monofs_synth_entry *entries;
	size_t count, i;

	entries = monofs_get_dir_entries(kind, &count);
	for (i = 0; i < count; i++) {
		if (strlen(entries[i].name) != name->len)
			continue;
		if (!memcmp(entries[i].name, name->name, name->len))
			return &entries[i];
	}

	return NULL;
}

static unsigned int monofs_mode_to_dtype(u32 mode)
{
	switch (mode & S_IFMT) {
	case S_IFDIR:
		return DT_DIR;
	case S_IFREG:
		return DT_REG;
	default:
		return DT_UNKNOWN;
	}
}

static bool monofs_is_remote_cacheable_kind(enum monofs_inode_kind kind)
{
	return kind == MONOFS_INODE_ROOT ||
	       kind == MONOFS_INODE_REMOTE_DIR ||
	       kind == MONOFS_INODE_REMOTE_FILE;
}

static void monofs_attach_dentry(struct dentry *dentry,
				 struct monofs_fs_info *fsi)
{
	d_set_d_op(dentry, &monofs_dentry_operations);
	dentry->d_time = (unsigned long)(fsi ? fsi->native_namespace_generation : 0);
}

static int monofs_d_revalidate(struct dentry *dentry, unsigned int flags)
{
	struct monofs_fs_info *fsi = dentry->d_sb->s_fs_info;
	struct inode *inode = d_inode(dentry);
	struct monofs_inode_ctx *ctx = inode ? monofs_inode_ctx(inode) : NULL;
	u64 generation;
	int ret;

	(void)flags;

	if (flags & LOOKUP_RCU)
		return -ECHILD;
	if (!monofs_native_connected(fsi))
		return 1;
	if (inode && (!ctx || !monofs_is_remote_cacheable_kind(ctx->kind)))
		return 1;

	ret = monofs_native_ping(fsi, &generation);
	if (ret)
		return ret;

	if (dentry == dentry->d_sb->s_root) {
		dentry->d_time = (unsigned long)generation;
		if (ctx)
			ctx->generation = generation;
		return 1;
	}

	if ((u64)dentry->d_time != generation)
		return 0;
	if (ctx && ctx->has_object_id && ctx->generation != generation)
		return 0;
	return 1;
}

const struct dentry_operations monofs_dentry_operations = {
	.d_revalidate = monofs_d_revalidate,
};

static u64 monofs_namespace_ino(const char *path)
{
	u64 hash = 1469598103934665603ULL;

	while (*path) {
		hash ^= (unsigned char)*path;
		hash *= 1099511628211ULL;
		path++;
	}

	return 0x100000000ULL | (hash & 0x7fffffffULL);
}

static int monofs_namespace_child_ino(const char *parent_path,
				      const char *child_name,
				      u64 *ino)
{
	char *candidate_path;

	if (!parent_path[0]) {
		*ino = monofs_namespace_ino(child_name);
		return 0;
	}

	candidate_path = kasprintf(GFP_KERNEL, "%s/%s", parent_path, child_name);
	if (!candidate_path)
		return -ENOMEM;

	*ino = monofs_namespace_ino(candidate_path);
	kfree(candidate_path);
	return 0;
}

static bool monofs_path_next_component(const char *seed_path,
				       const char *parent_path,
				       const char **child_name,
				       size_t *child_len)
{
	size_t parent_len = strlen(parent_path);
	const char *start;
	const char *slash;

	if (!parent_len) {
		start = seed_path;
	} else {
		if (strcmp(seed_path, parent_path) == 0)
			return false;
		if (strncmp(seed_path, parent_path, parent_len) != 0 ||
		    seed_path[parent_len] != '/')
			return false;
		start = seed_path + parent_len + 1;
	}

	if (!*start)
		return false;

	slash = strchr(start, '/');
	*child_name = start;
	*child_len = slash ? (size_t)(slash - start) : strlen(start);
	return true;
}

static bool monofs_is_seed_or_parent(const struct monofs_fs_info *fsi,
				     const char *candidate_path)
{
	u32 i;
	size_t len = strlen(candidate_path);

	for (i = 0; i < fsi->mount_opts.seed_path_count; i++) {
		const char *seed_path = fsi->mount_opts.seed_paths[i];

		if (!strcmp(seed_path, candidate_path))
			return true;
		if (len && !strncmp(seed_path, candidate_path, len) &&
		    seed_path[len] == '/')
			return true;
	}

	return false;
}

static int monofs_emit_remote_children(struct dir_context *ctx,
				       struct monofs_fs_info *fsi,
				       const char *parent_path,
				       const u8 object_id[MONOFS_NATIVE_OBJECT_ID_LEN],
				       u64 cookie)
{
	struct monofs_native_readdir_reply reply;
	u32 i;
	int ret;

	(void)parent_path;

	ret = monofs_native_readdir(fsi, object_id, cookie, 256, &reply);
	if (ret)
		return ret;

	for (i = 0; i < reply.entry_count; i++) {
		struct monofs_native_readdir_entry *entry = &reply.entries[i];

		if (!dir_emit(ctx, entry->name, strlen(entry->name),
			      entry->ino ? entry->ino : monofs_namespace_ino(entry->name),
			      monofs_mode_to_dtype(entry->mode)))
			break;
		ctx->pos++;
	}

	monofs_native_readdir_free(&reply);
	return 0;
}

static int monofs_emit_namespace_children(struct dir_context *ctx,
					  const struct monofs_fs_info *fsi,
					  const char *parent_path,
					  u64 skip)
{
	char **seen;
	u32 seen_count = 0;
	u32 i;
	int ret = 0;
	u64 emitted = 0;

	seen = kcalloc(fsi->mount_opts.seed_path_count, sizeof(*seen), GFP_KERNEL);
	if (!seen && fsi->mount_opts.seed_path_count)
		return -ENOMEM;

	for (i = 0; i < fsi->mount_opts.seed_path_count; i++) {
		const char *child_name;
		size_t child_len;
		u64 ino;
		u32 j;
		bool duplicate = false;

		if (!monofs_path_next_component(fsi->mount_opts.seed_paths[i], parent_path,
						&child_name, &child_len))
			continue;

		for (j = 0; j < seen_count; j++) {
			if (strlen(seen[j]) == child_len &&
			    !memcmp(seen[j], child_name, child_len)) {
				duplicate = true;
				break;
			}
		}
		if (duplicate)
			continue;

		seen[seen_count] = kmalloc(child_len + 1, GFP_KERNEL);
		if (!seen[seen_count]) {
			ret = -ENOMEM;
			goto out;
		}
		memcpy(seen[seen_count], child_name, child_len);
		seen[seen_count][child_len] = '\0';
		seen_count++;

		if (emitted++ < skip)
			continue;

		ret = monofs_namespace_child_ino(parent_path, seen[seen_count - 1], &ino);
		if (ret)
			goto out;

		if (!dir_emit(ctx, child_name, child_len, ino, DT_DIR))
			goto out;
		ctx->pos++;
	}

out:
	while (seen_count > 0) {
		seen_count--;
		kfree(seen[seen_count]);
	}
	kfree(seen);
	return ret;
}

static ssize_t monofs_control_read(struct file *file, char __user *buf,
				   size_t len, loff_t *ppos)
{
	struct monofs_fs_info *fsi = file_inode(file)->i_sb->s_fs_info;
	struct monofs_inode_ctx *ctx = monofs_inode_ctx(file_inode(file));
	char *content;
	ssize_t ret;
	u32 i;

	if (!ctx)
		return -EIO;

	switch (ctx->kind) {
	case MONOFS_INODE_STATUS_FILE:
		content = kasprintf(GFP_KERNEL,
				    "filesystem=%s\n"
				    "gateway=%s\n"
				    "cluster_version=%llu\n"
				    "namespace_generation=%llu\n"
				    "overlay_writes=%u\n"
				    "auth_token=%s\n"
				    "attr_ttl_ms=%u\n"
				    "debug=%u\n"
				    "seed_path_count=%u\n",
				    MONOFS_NAME,
				    fsi->mount_opts.gateway ? fsi->mount_opts.gateway : "<unset>",
				    (unsigned long long)fsi->cluster_version,
				    (unsigned long long)fsi->native_namespace_generation,
				    fsi->mount_opts.overlay_writes,
				    fsi->mount_opts.auth_token ? "<set>" : "<unset>",
				    fsi->mount_opts.attr_ttl_ms,
				    fsi->mount_opts.debug,
				    fsi->mount_opts.seed_path_count);
		if (content) {
			char *next = kasprintf(GFP_KERNEL, "%snative_connected=%u\nnative_session_id=%llu\n",
					       content,
					       fsi->native_connected,
					       (unsigned long long)fsi->native_session_id);

			kfree(content);
			content = next;
		}
		break;
	case MONOFS_INODE_SEEDS_FILE:
		if (!fsi->mount_opts.seed_path_count) {
			content = kstrdup("<none>\n", GFP_KERNEL);
			break;
		}
		{
			size_t total_len = 1;
			size_t offset = 0;

			for (i = 0; i < fsi->mount_opts.seed_path_count; i++)
				total_len += strlen(fsi->mount_opts.seed_paths[i]) + 1;

			content = kzalloc(total_len, GFP_KERNEL);
			if (!content)
				return -ENOMEM;

			for (i = 0; i < fsi->mount_opts.seed_path_count; i++)
				offset += scnprintf(content + offset, total_len - offset,
						   "%s\n", fsi->mount_opts.seed_paths[i]);
		}
		break;
	default:
		return -EINVAL;
	}

	if (!content)
		return -ENOMEM;

	ret = simple_read_from_buffer(buf, len, ppos, content, strlen(content));
	kfree(content);
	return ret;
}

static int monofs_iterate_shared(struct file *file, struct dir_context *ctx)
{
	struct inode *inode = file_inode(file);
	struct monofs_inode_ctx *ictx = monofs_inode_ctx(inode);
	struct monofs_fs_info *fsi = inode->i_sb->s_fs_info;
	enum monofs_inode_kind kind = monofs_inode_kind(inode);
	const struct monofs_synth_entry *entries;
	size_t count, index;
	int ret;
	u64 dynamic_skip;

	if (!ictx)
		return -EIO;

	entries = monofs_get_dir_entries(kind, &count);
	if (!entries && kind != MONOFS_INODE_ROOT && kind != MONOFS_INODE_CONTROL_DIR &&
	    kind != MONOFS_INODE_NAMESPACE_DIR && kind != MONOFS_INODE_REMOTE_DIR)
		return -ENOTDIR;

	if (!dir_emit_dots(file, ctx))
		return 0;

	for (index = ctx->pos >= 2 ? ctx->pos - 2 : 0; index < count; index++) {
		if (!dir_emit(ctx, entries[index].name, strlen(entries[index].name),
			      entries[index].ino, entries[index].d_type))
			return 0;
		ctx->pos++;
	}

	if ((kind == MONOFS_INODE_ROOT || kind == MONOFS_INODE_REMOTE_DIR) &&
	    monofs_native_connected(fsi) && ictx->has_object_id) {
		dynamic_skip = ctx->pos >= 2 + count ? ctx->pos - 2 - count : 0;
		ret = monofs_emit_remote_children(ctx, fsi, ictx->path, ictx->object_id,
						  dynamic_skip);
		if (ret)
			return ret;
		ictx->generation = fsi->native_namespace_generation;
		file->f_path.dentry->d_time = (unsigned long)fsi->native_namespace_generation;
		return 0;
	}

	if (kind == MONOFS_INODE_ROOT || kind == MONOFS_INODE_NAMESPACE_DIR) {
		dynamic_skip = ctx->pos >= 2 + count ? ctx->pos - 2 - count : 0;
		ret = monofs_emit_namespace_children(ctx, fsi, ictx->path, dynamic_skip);
		if (ret)
			return ret;
	}

	return 0;
}

static struct dentry *monofs_lookup(struct inode *dir, struct dentry *dentry,
				    unsigned int flags)
{
	struct monofs_inode_ctx *parent_ctx = monofs_inode_ctx(dir);
	struct monofs_fs_info *fsi = dir->i_sb->s_fs_info;
	const struct monofs_synth_entry *entry;
	char *candidate_path = NULL;
	struct inode *inode;
	int ret;

	(void)flags;

	if (!parent_ctx)
		return ERR_PTR(-EIO);

	if (parent_ctx->kind == MONOFS_INODE_ROOT ||
	    parent_ctx->kind == MONOFS_INODE_CONTROL_DIR) {
		entry = monofs_find_child(parent_ctx->kind, &dentry->d_name);
		if (entry) {
			monofs_attach_dentry(dentry, fsi);
			inode = monofs_get_inode(dir->i_sb, dir, entry->mode,
						 entry->ino, entry->kind, entry->name, NULL);
			if (!inode)
				return ERR_PTR(-ENOMEM);
			d_add(dentry, inode);
			return NULL;
		}
	}

	if ((parent_ctx->kind == MONOFS_INODE_ROOT ||
	     parent_ctx->kind == MONOFS_INODE_REMOTE_DIR) &&
	    monofs_native_connected(fsi) && parent_ctx->has_object_id) {
		struct monofs_native_lookup_reply reply;
		enum monofs_inode_kind child_kind;
		const char *lookup_name;

		if (dentry->d_name.len == 0) {
			monofs_attach_dentry(dentry, fsi);
			d_add(dentry, NULL);
			return NULL;
		}

		if (!parent_ctx->path[0])
			candidate_path = kasprintf(GFP_KERNEL, "%.*s", dentry->d_name.len,
						   dentry->d_name.name);
		else
			candidate_path = kasprintf(GFP_KERNEL, "%s/%.*s", parent_ctx->path,
						   dentry->d_name.len, dentry->d_name.name);
		if (!candidate_path)
			return ERR_PTR(-ENOMEM);

		lookup_name = candidate_path + strlen(parent_ctx->path) +
			     (parent_ctx->path[0] ? 1 : 0);
		ret = monofs_native_lookup(fsi, parent_ctx->object_id, lookup_name, &reply);
		if (ret) {
			kfree(candidate_path);
			return ERR_PTR(ret);
		}
		monofs_attach_dentry(dentry, fsi);
		if (!reply.found) {
			kfree(candidate_path);
			d_add(dentry, NULL);
			return NULL;
		}

		child_kind = (reply.attr.mode & S_IFMT) == S_IFDIR ?
			MONOFS_INODE_REMOTE_DIR : MONOFS_INODE_REMOTE_FILE;
		inode = monofs_get_inode(dir->i_sb, dir, reply.attr.mode,
					 reply.attr.ino ? reply.attr.ino : monofs_namespace_ino(candidate_path),
					 child_kind, candidate_path, reply.object_id);
		kfree(candidate_path);
		if (!inode)
			return ERR_PTR(-ENOMEM);
		monofs_apply_native_attr(inode, &reply.attr);
		d_add(dentry, inode);
		return NULL;
	}

	if (parent_ctx->kind != MONOFS_INODE_ROOT &&
	    parent_ctx->kind != MONOFS_INODE_NAMESPACE_DIR) {
		monofs_attach_dentry(dentry, fsi);
		d_add(dentry, NULL);
		return NULL;
	}

	if (!parent_ctx->path[0])
		candidate_path = kasprintf(GFP_KERNEL, "%.*s", dentry->d_name.len,
					   dentry->d_name.name);
	else
		candidate_path = kasprintf(GFP_KERNEL, "%s/%.*s", parent_ctx->path,
					   dentry->d_name.len, dentry->d_name.name);
	if (!candidate_path)
		return ERR_PTR(-ENOMEM);

	if (!strcmp(candidate_path, ".monofs")) {
		monofs_attach_dentry(dentry, fsi);
		inode = monofs_get_inode(dir->i_sb, dir, S_IFDIR | 0555,
					 MONOFS_CONTROL_INO, MONOFS_INODE_CONTROL_DIR,
					 candidate_path, NULL);
		kfree(candidate_path);
		if (!inode)
			return ERR_PTR(-ENOMEM);
		d_add(dentry, inode);
		return NULL;
	}

	if (!monofs_is_seed_or_parent(fsi, candidate_path)) {
		kfree(candidate_path);
		monofs_attach_dentry(dentry, fsi);
		d_add(dentry, NULL);
		return NULL;
	}

	monofs_attach_dentry(dentry, fsi);
	inode = monofs_get_inode(dir->i_sb, dir, S_IFDIR | 0555,
				 monofs_namespace_ino(candidate_path),
				 MONOFS_INODE_NAMESPACE_DIR, candidate_path, NULL);
	kfree(candidate_path);
	if (!inode)
		return ERR_PTR(-ENOMEM);
	d_add(dentry, inode);
	return NULL;
}

const struct inode_operations monofs_dir_inode_operations = {
	.lookup = monofs_lookup,
};

const struct file_operations monofs_dir_operations = {
	.llseek = generic_file_llseek,
	.read = generic_read_dir,
	.iterate_shared = monofs_iterate_shared,
};

const struct file_operations monofs_control_file_operations = {
	.open = simple_open,
	.read = monofs_control_read,
	.llseek = default_llseek,
};
