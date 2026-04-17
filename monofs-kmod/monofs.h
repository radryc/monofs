/* SPDX-License-Identifier: GPL-2.0-only */

#ifndef MONOFS_KMOD_MONOFS_H
#define MONOFS_KMOD_MONOFS_H

#include <linux/fs.h>
#include <linux/fs_context.h>
#include <linux/fs_parser.h>
#include <linux/mutex.h>
#include <linux/types.h>

#define MONOFS_NAME "monofs"
#define MONOFS_MAGIC 0x6d6f6e6f
#define MONOFS_DEFAULT_ATTR_TTL_MS 1000U
#define MONOFS_NATIVE_OBJECT_ID_LEN 16
#define MONOFS_ROOT_INO 1
#define MONOFS_CONTROL_INO 2
#define MONOFS_STATUS_INO 3
#define MONOFS_SEEDS_INO 4

enum monofs_inode_kind {
	MONOFS_INODE_NONE = 0,
	MONOFS_INODE_ROOT,
	MONOFS_INODE_CONTROL_DIR,
	MONOFS_INODE_STATUS_FILE,
	MONOFS_INODE_SEEDS_FILE,
	MONOFS_INODE_NAMESPACE_DIR,
	MONOFS_INODE_REMOTE_DIR,
	MONOFS_INODE_REMOTE_FILE,
};

struct monofs_mount_opts {
	char *gateway;
	char *auth_token;
	char **seed_paths;
	u32 seed_path_count;
	u32 attr_ttl_ms;
	bool overlay_writes;
	bool debug;
};

struct monofs_native_attr {
	u64 ino;
	u32 mode;
	u64 size;
	s64 mtime;
	s64 atime;
	s64 ctime;
	u32 nlink;
	u32 uid;
	u32 gid;
};

struct monofs_native_lookup_reply {
	bool found;
	u32 entry_ttl_ms;
	u8 object_id[MONOFS_NATIVE_OBJECT_ID_LEN];
	struct monofs_native_attr attr;
};

struct monofs_native_readdir_entry {
	char *name;
	u8 object_id[MONOFS_NATIVE_OBJECT_ID_LEN];
	u64 ino;
	u32 mode;
};

struct monofs_native_readdir_reply {
	u32 dir_ttl_ms;
	u64 next_cookie;
	bool eof;
	u32 entry_count;
	struct monofs_native_readdir_entry *entries;
};

struct monofs_native_statfs_reply {
	u64 blocks;
	u64 bfree;
	u64 bavail;
	u64 files;
	u64 ffree;
	u32 bsize;
	u32 frsize;
	u32 name_len;
};

struct monofs_fs_info {
	struct monofs_mount_opts mount_opts;
	u64 cluster_version;
	u64 native_session_id;
	u64 native_next_request_id;
	u64 native_namespace_generation;
	u8 native_root_object_id[MONOFS_NATIVE_OBJECT_ID_LEN];
	u32 native_entry_ttl_ms;
	u32 native_attr_ttl_ms;
	u32 native_dir_ttl_ms;
	u32 native_route_ttl_ms;
	bool native_connected;
	struct socket *native_sock;
	struct mutex native_lock;
	struct monofs_native_attr native_root_attr;
};

struct monofs_inode_ctx {
	enum monofs_inode_kind kind;
	char *path;
	u8 object_id[MONOFS_NATIVE_OBJECT_ID_LEN];
	bool has_object_id;
	u64 generation;
	u64 read_handle;
	bool has_read_handle;
	struct mutex read_handle_lock;
};

extern const struct fs_parameter_spec monofs_fs_parameters[];
extern struct file_system_type monofs_fs_type;
extern const struct dentry_operations monofs_dentry_operations;
extern const struct inode_operations monofs_dir_inode_operations;
extern const struct file_operations monofs_dir_operations;
extern const struct file_operations monofs_control_file_operations;
extern const struct file_operations monofs_remote_file_operations;
extern const struct address_space_operations monofs_remote_aops;

int monofs_init_fs_context(struct fs_context *fc);
int monofs_fill_super(struct super_block *sb, struct fs_context *fc);
void monofs_kill_sb(struct super_block *sb);
void monofs_free_fs_info(struct monofs_fs_info *fsi);
struct inode *monofs_get_inode(struct super_block *sb,
			       const struct inode *dir,
			       umode_t mode,
			       u64 ino,
			       enum monofs_inode_kind kind,
			       const char *path,
			       const u8 *object_id);
void monofs_apply_native_attr(struct inode *inode,
			      const struct monofs_native_attr *attr);

int monofs_native_mount(struct monofs_fs_info *fsi);
void monofs_native_disconnect(struct monofs_fs_info *fsi);
int monofs_native_lookup(struct monofs_fs_info *fsi,
			 const u8 parent_object_id[MONOFS_NATIVE_OBJECT_ID_LEN],
			 const char *name,
			 struct monofs_native_lookup_reply *reply);
int monofs_native_readdir(struct monofs_fs_info *fsi,
			  const u8 dir_object_id[MONOFS_NATIVE_OBJECT_ID_LEN],
			  u64 cookie,
			  u32 max_entries,
			  struct monofs_native_readdir_reply *reply);
void monofs_native_readdir_free(struct monofs_native_readdir_reply *reply);
int monofs_native_statfs(struct monofs_fs_info *fsi,
			 struct monofs_native_statfs_reply *reply);
int monofs_native_ping(struct monofs_fs_info *fsi, u64 *generation);
int monofs_native_open(struct monofs_fs_info *fsi,
		       const u8 object_id[MONOFS_NATIVE_OBJECT_ID_LEN],
		       u64 *handle_id,
		       struct monofs_native_attr *attr);
int monofs_native_read(struct monofs_fs_info *fsi,
		       u64 handle_id,
		       u64 offset,
		       u32 length,
		       void *data,
		       u32 *bytes_read,
		       bool *eof);
int monofs_native_close(struct monofs_fs_info *fsi, u64 handle_id);

static inline bool monofs_native_connected(const struct monofs_fs_info *fsi)
{
	return fsi && fsi->native_connected;
}

static inline struct monofs_inode_ctx *monofs_inode_ctx(const struct inode *inode)
{
	return inode->i_private;
}

static inline enum monofs_inode_kind monofs_inode_kind(const struct inode *inode)
{
	struct monofs_inode_ctx *ctx = monofs_inode_ctx(inode);

	return ctx ? ctx->kind : MONOFS_INODE_NONE;
}

#endif /* MONOFS_KMOD_MONOFS_H */
