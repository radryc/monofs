// SPDX-License-Identifier: GPL-2.0-only

#include <linux/errno.h>
#include <linux/in.h>
#include <linux/inet.h>
#include <linux/kernel.h>
#include <linux/net.h>
#include <linux/slab.h>
#include <linux/string.h>
#include <linux/uio.h>
#include <net/net_namespace.h>
#include <net/sock.h>

#include "monofs.h"

#define MONOFS_NATIVE_MAGIC 0x53464e4dU
#define MONOFS_NATIVE_VERSION 1U
#define MONOFS_NATIVE_HEADER_LEN 52U
#define MONOFS_NATIVE_OPCODE_HELLO 0x0001U
#define MONOFS_NATIVE_OPCODE_MOUNT 0x0002U
#define MONOFS_NATIVE_OPCODE_LOOKUP 0x0010U
#define MONOFS_NATIVE_OPCODE_GETATTR 0x0011U
#define MONOFS_NATIVE_OPCODE_READDIR 0x0012U
#define MONOFS_NATIVE_OPCODE_STATFS 0x0014U
#define MONOFS_NATIVE_OPCODE_OPEN_READ 0x0020U
#define MONOFS_NATIVE_OPCODE_READ 0x0021U
#define MONOFS_NATIVE_OPCODE_CLOSE 0x0022U
#define MONOFS_NATIVE_OPCODE_PING 0x0031U
#define MONOFS_NATIVE_STATUS_OK 0U
#define MONOFS_NATIVE_STATUS_INVALID_REQUEST 1U
#define MONOFS_NATIVE_STATUS_AUTH 2U
#define MONOFS_NATIVE_STATUS_NOT_FOUND 3U
#define MONOFS_NATIVE_STATUS_NOT_DIR 4U
#define MONOFS_NATIVE_STATUS_IS_DIR 5U
#define MONOFS_NATIVE_STATUS_STALE_NAMESPACE 6U
#define MONOFS_NATIVE_STATUS_STALE_ROUTE 7U
#define MONOFS_NATIVE_STATUS_UNAVAILABLE 8U
#define MONOFS_NATIVE_STATUS_BACKEND_IO 9U
#define MONOFS_NATIVE_STATUS_CANCELLED 10U
#define MONOFS_NATIVE_STATUS_UNSUPPORTED 11U
#define MONOFS_NATIVE_MOUNT_FLAG_READONLY (1U << 0)
#define MONOFS_NATIVE_MOUNT_FLAG_OVERLAY_WRITES (1U << 1)
#define MONOFS_NATIVE_MOUNT_FLAG_DEBUG (1U << 2)

struct monofs_native_frame {
	__le32 magic;
	__le16 version;
	__le16 opcode;
	__le32 flags;
	__le32 header_len;
	__le32 body_len;
	__le64 request_id;
	__le64 session_id;
	__le32 status;
	__le32 reserved;
	__le64 generation;
} __packed;

struct monofs_native_cursor {
	u8 *data;
	size_t len;
	size_t off;
};

static int monofs_native_status_to_errno(u32 status)
{
	switch (status) {
	case MONOFS_NATIVE_STATUS_OK:
		return 0;
	case MONOFS_NATIVE_STATUS_INVALID_REQUEST:
		return -EINVAL;
	case MONOFS_NATIVE_STATUS_AUTH:
		return -EACCES;
	case MONOFS_NATIVE_STATUS_NOT_FOUND:
		return -ENOENT;
	case MONOFS_NATIVE_STATUS_NOT_DIR:
		return -ENOTDIR;
	case MONOFS_NATIVE_STATUS_IS_DIR:
		return -EISDIR;
	case MONOFS_NATIVE_STATUS_STALE_NAMESPACE:
	case MONOFS_NATIVE_STATUS_STALE_ROUTE:
		return -ESTALE;
	case MONOFS_NATIVE_STATUS_CANCELLED:
		return -EINTR;
	case MONOFS_NATIVE_STATUS_UNSUPPORTED:
		return -EOPNOTSUPP;
	default:
		return -EIO;
	}
}

static int monofs_native_cursor_require(struct monofs_native_cursor *cursor, size_t needed)
{
	if (cursor->off + needed > cursor->len)
		return -EIO;
	return 0;
}

static int monofs_native_put_u16(struct monofs_native_cursor *cursor, u16 value)
{
	__le16 tmp = cpu_to_le16(value);
	int ret = monofs_native_cursor_require(cursor, sizeof(tmp));

	if (ret)
		return ret;
	memcpy(cursor->data + cursor->off, &tmp, sizeof(tmp));
	cursor->off += sizeof(tmp);
	return 0;
}

static int monofs_native_put_u32(struct monofs_native_cursor *cursor, u32 value)
{
	__le32 tmp = cpu_to_le32(value);
	int ret = monofs_native_cursor_require(cursor, sizeof(tmp));

	if (ret)
		return ret;
	memcpy(cursor->data + cursor->off, &tmp, sizeof(tmp));
	cursor->off += sizeof(tmp);
	return 0;
}

static int monofs_native_put_u64(struct monofs_native_cursor *cursor, u64 value)
{
	__le64 tmp = cpu_to_le64(value);
	int ret = monofs_native_cursor_require(cursor, sizeof(tmp));

	if (ret)
		return ret;
	memcpy(cursor->data + cursor->off, &tmp, sizeof(tmp));
	cursor->off += sizeof(tmp);
	return 0;
}

static int monofs_native_put_bytes(struct monofs_native_cursor *cursor,
				       const void *data, size_t len)
{
	int ret = monofs_native_cursor_require(cursor, len);

	if (ret)
		return ret;
	memcpy(cursor->data + cursor->off, data, len);
	cursor->off += len;
	return 0;
}

static int monofs_native_put_string(struct monofs_native_cursor *cursor, const char *value)
{
	size_t len = value ? strlen(value) : 0;
	int ret;

	ret = monofs_native_put_u32(cursor, (u32)len);
	if (ret)
		return ret;
	return monofs_native_put_bytes(cursor, value, len);
}

static int monofs_native_get_u8(struct monofs_native_cursor *cursor, u8 *value)
{
	int ret = monofs_native_cursor_require(cursor, sizeof(*value));

	if (ret)
		return ret;
	*value = cursor->data[cursor->off++];
	return 0;
}

static int monofs_native_get_u32(struct monofs_native_cursor *cursor, u32 *value)
{
	__le32 tmp;
	int ret = monofs_native_cursor_require(cursor, sizeof(tmp));

	if (ret)
		return ret;
	memcpy(&tmp, cursor->data + cursor->off, sizeof(tmp));
	cursor->off += sizeof(tmp);
	*value = le32_to_cpu(tmp);
	return 0;
}

static int monofs_native_get_u64(struct monofs_native_cursor *cursor, u64 *value)
{
	__le64 tmp;
	int ret = monofs_native_cursor_require(cursor, sizeof(tmp));

	if (ret)
		return ret;
	memcpy(&tmp, cursor->data + cursor->off, sizeof(tmp));
	cursor->off += sizeof(tmp);
	*value = le64_to_cpu(tmp);
	return 0;
}

static int monofs_native_get_i64(struct monofs_native_cursor *cursor, s64 *value)
{
	u64 tmp;
	int ret = monofs_native_get_u64(cursor, &tmp);

	if (ret)
		return ret;
	*value = (s64)tmp;
	return 0;
}

static int monofs_native_get_bytes(struct monofs_native_cursor *cursor, void *data, size_t len)
{
	int ret = monofs_native_cursor_require(cursor, len);

	if (ret)
		return ret;
	memcpy(data, cursor->data + cursor->off, len);
	cursor->off += len;
	return 0;
}

static int monofs_native_get_string(struct monofs_native_cursor *cursor, char **value)
{
	u32 len;
	char *str;
	int ret;

	ret = monofs_native_get_u32(cursor, &len);
	if (ret)
		return ret;
	ret = monofs_native_cursor_require(cursor, len);
	if (ret)
		return ret;

	str = kmalloc(len + 1, GFP_KERNEL);
	if (!str)
		return -ENOMEM;
	memcpy(str, cursor->data + cursor->off, len);
	str[len] = '\0';
	cursor->off += len;
	*value = str;
	return 0;
}

static int monofs_native_get_attr(struct monofs_native_cursor *cursor,
				      struct monofs_native_attr *attr)
{
	int ret;

	ret = monofs_native_get_u64(cursor, &attr->ino);
	if (ret)
		return ret;
	ret = monofs_native_get_u32(cursor, &attr->mode);
	if (ret)
		return ret;
	ret = monofs_native_get_u64(cursor, &attr->size);
	if (ret)
		return ret;
	ret = monofs_native_get_i64(cursor, &attr->mtime);
	if (ret)
		return ret;
	ret = monofs_native_get_i64(cursor, &attr->atime);
	if (ret)
		return ret;
	ret = monofs_native_get_i64(cursor, &attr->ctime);
	if (ret)
		return ret;
	ret = monofs_native_get_u32(cursor, &attr->nlink);
	if (ret)
		return ret;
	ret = monofs_native_get_u32(cursor, &attr->uid);
	if (ret)
		return ret;
	return monofs_native_get_u32(cursor, &attr->gid);
}

static int monofs_native_send_all(struct socket *sock, const void *data, size_t len)
{
	struct msghdr msg = {};
	struct kvec iov = {
		.iov_base = (void *)data,
		.iov_len = len,
	};
	int ret = kernel_sendmsg(sock, &msg, &iov, 1, len);

	if (ret < 0)
		return ret;
	if (ret != len)
		return -EIO;
	return 0;
}

static int monofs_native_recv_all(struct socket *sock, void *data, size_t len)
{
	size_t done = 0;

	while (done < len) {
		struct msghdr msg = {};
		struct kvec iov = {
			.iov_base = (u8 *)data + done,
			.iov_len = len - done,
		};
		int ret = kernel_recvmsg(sock, &msg, &iov, 1, len - done, 0);

		if (ret < 0)
			return ret;
		if (ret == 0)
			return -EIO;
		done += ret;
	}

	return 0;
}

static int monofs_native_roundtrip(struct monofs_fs_info *fsi,
				       u16 opcode,
				       const void *request_body,
				       size_t request_len,
				       struct monofs_native_frame *reply_hdr,
				       u8 **reply_body)
{
	struct monofs_native_frame req = {
		.magic = cpu_to_le32(MONOFS_NATIVE_MAGIC),
		.version = cpu_to_le16(MONOFS_NATIVE_VERSION),
		.opcode = cpu_to_le16(opcode),
		.flags = cpu_to_le32(0),
		.header_len = cpu_to_le32(MONOFS_NATIVE_HEADER_LEN),
		.body_len = cpu_to_le32(request_len),
		.request_id = cpu_to_le64(++fsi->native_next_request_id),
		.session_id = cpu_to_le64(fsi->native_session_id),
		.status = cpu_to_le32(0),
		.reserved = cpu_to_le32(0),
		.generation = cpu_to_le64(fsi->native_namespace_generation),
	};
	u8 *body = NULL;
	int ret;

	if (!fsi->native_sock)
		return -ENOTCONN;

	mutex_lock(&fsi->native_lock);
	ret = monofs_native_send_all(fsi->native_sock, &req, sizeof(req));
	if (ret)
		goto out;
	if (request_len > 0) {
		ret = monofs_native_send_all(fsi->native_sock, request_body, request_len);
		if (ret)
			goto out;
	}

	ret = monofs_native_recv_all(fsi->native_sock, reply_hdr, sizeof(*reply_hdr));
	if (ret)
		goto out;

	if (le32_to_cpu(reply_hdr->magic) != MONOFS_NATIVE_MAGIC ||
	    le16_to_cpu(reply_hdr->version) != MONOFS_NATIVE_VERSION ||
	    le32_to_cpu(reply_hdr->header_len) != MONOFS_NATIVE_HEADER_LEN) {
		ret = -EPROTO;
		goto out;
	}
	fsi->native_namespace_generation = le64_to_cpu(reply_hdr->generation);

	if (le32_to_cpu(reply_hdr->body_len) > 0) {
		body = kmalloc(le32_to_cpu(reply_hdr->body_len), GFP_KERNEL);
		if (!body) {
			ret = -ENOMEM;
			goto out;
		}
		ret = monofs_native_recv_all(fsi->native_sock, body,
					       le32_to_cpu(reply_hdr->body_len));
		if (ret)
			goto out;
	}

	ret = monofs_native_status_to_errno(le32_to_cpu(reply_hdr->status));
	if (ret)
		goto out;

	*reply_body = body;
	body = NULL;
out:
	mutex_unlock(&fsi->native_lock);
	kfree(body);
	return ret;
}

static int monofs_native_parse_gateway(const char *gateway, struct sockaddr_in *sin)
{
	char *work;
	char *colon;
	char *host;
	char *port_str;
	u8 addr_bytes[4];
	u16 port;
	int ret = 0;

	if (!gateway || !*gateway)
		return -EINVAL;

	work = kstrdup(gateway, GFP_KERNEL);
	if (!work)
		return -ENOMEM;

	colon = strrchr(work, ':');
	if (!colon) {
		ret = -EINVAL;
		goto out;
	}
	*colon = '\0';
	host = work;
	port_str = colon + 1;

	if (!strcmp(host, "localhost"))
		host = "127.0.0.1";

	if (!in4_pton(host, -1, addr_bytes, '\0', NULL)) {
		ret = -EINVAL;
		goto out;
	}
	ret = kstrtou16(port_str, 10, &port);
	if (ret)
		goto out;

	memset(sin, 0, sizeof(*sin));
	sin->sin_family = AF_INET;
	sin->sin_port = htons(port);
	memcpy(&sin->sin_addr.s_addr, addr_bytes, sizeof(addr_bytes));
out:
	kfree(work);
	return ret;
}

static int monofs_native_send_hello(struct monofs_fs_info *fsi)
{
	static const char client_kind[] = "kmod";
	static const char client_version[] = "scaffold";
	struct monofs_native_frame reply_hdr;
	struct monofs_native_cursor cursor;
	u8 *body;
	u8 *reply_body = NULL;
	int ret;
	size_t body_len = 2 + 2 + 8 +
			  4 + sizeof(client_kind) - 1 +
			  4 + sizeof(client_version) - 1 +
			  4;

	body = kmalloc(body_len, GFP_KERNEL);
	if (!body)
		return -ENOMEM;

	cursor.data = body;
	cursor.len = body_len;
	cursor.off = 0;

	ret = monofs_native_put_u16(&cursor, MONOFS_NATIVE_VERSION);
	if (!ret)
		ret = monofs_native_put_u16(&cursor, MONOFS_NATIVE_VERSION);
	if (!ret)
		ret = monofs_native_put_u64(&cursor, 0);
	if (!ret)
		ret = monofs_native_put_string(&cursor, client_kind);
	if (!ret)
		ret = monofs_native_put_string(&cursor, client_version);
	if (!ret)
		ret = monofs_native_put_string(&cursor, "");
	if (ret)
		goto out;

	ret = monofs_native_roundtrip(fsi, MONOFS_NATIVE_OPCODE_HELLO,
					body, cursor.off, &reply_hdr, &reply_body);
out:
	kfree(body);
	kfree(reply_body);
	return ret;
}

int monofs_native_mount(struct monofs_fs_info *fsi)
{
	struct sockaddr_in sin;
	struct socket *sock = NULL;
	struct monofs_native_frame reply_hdr;
	struct monofs_native_cursor cursor;
	u8 *body;
	u8 *reply_body = NULL;
	int ret;
	size_t auth_len = fsi->mount_opts.auth_token ? strlen(fsi->mount_opts.auth_token) : 0;
	u32 mount_flags = MONOFS_NATIVE_MOUNT_FLAG_READONLY;
	size_t body_len = 4 +
			  4 + sizeof("monofs-kmod") - 1 +
			  4 +
			  4 + auth_len;

	ret = monofs_native_parse_gateway(fsi->mount_opts.gateway, &sin);
	if (ret)
		return ret;

	ret = sock_create_kern(&init_net, AF_INET, SOCK_STREAM, IPPROTO_TCP, &sock);
	if (ret)
		return ret;

	ret = kernel_connect(sock, (struct sockaddr *)&sin, sizeof(sin), 0);
	if (ret)
		goto out;

	fsi->native_sock = sock;
	mutex_init(&fsi->native_lock);
	fsi->native_next_request_id = 0;
	fsi->native_session_id = 0;

	ret = monofs_native_send_hello(fsi);
	if (ret)
		goto out;

	if (fsi->mount_opts.overlay_writes)
		mount_flags |= MONOFS_NATIVE_MOUNT_FLAG_OVERLAY_WRITES;
	if (fsi->mount_opts.debug)
		mount_flags |= MONOFS_NATIVE_MOUNT_FLAG_DEBUG;

	body = kmalloc(body_len, GFP_KERNEL);
	if (!body) {
		ret = -ENOMEM;
		goto out;
	}
	cursor.data = body;
	cursor.len = body_len;
	cursor.off = 0;

	ret = monofs_native_put_u32(&cursor, mount_flags);
	if (!ret)
		ret = monofs_native_put_string(&cursor, "monofs-kmod");
	if (!ret)
		ret = monofs_native_put_string(&cursor, "");
	if (!ret)
		ret = monofs_native_put_string(&cursor,
					       fsi->mount_opts.auth_token ? fsi->mount_opts.auth_token : "");
	if (ret) {
		kfree(body);
		goto out;
	}

	ret = monofs_native_roundtrip(fsi, MONOFS_NATIVE_OPCODE_MOUNT,
					body, cursor.off, &reply_hdr, &reply_body);
	kfree(body);
	if (ret)
		goto out;

	{
		struct monofs_native_cursor reply = {
			.data = reply_body,
			.len = le32_to_cpu(reply_hdr.body_len),
			.off = 0,
		};
		u8 guardian_visible;

		ret = monofs_native_get_u64(&reply, &fsi->cluster_version);
		if (!ret)
			ret = monofs_native_get_u64(&reply, &fsi->native_namespace_generation);
		if (!ret)
			ret = monofs_native_get_u8(&reply, &guardian_visible);
		if (!ret)
			ret = monofs_native_get_bytes(&reply, fsi->native_root_object_id,
						       MONOFS_NATIVE_OBJECT_ID_LEN);
		if (!ret)
			ret = monofs_native_get_attr(&reply, &fsi->native_root_attr);
		if (!ret)
			ret = monofs_native_get_u32(&reply, &fsi->native_entry_ttl_ms);
		if (!ret)
			ret = monofs_native_get_u32(&reply, &fsi->native_attr_ttl_ms);
		if (!ret)
			ret = monofs_native_get_u32(&reply, &fsi->native_dir_ttl_ms);
		if (!ret)
			ret = monofs_native_get_u32(&reply, &fsi->native_route_ttl_ms);
		if (ret)
			goto out;
	}

	fsi->native_session_id = le64_to_cpu(reply_hdr.session_id);
	fsi->native_connected = true;
	kfree(reply_body);
	return 0;

out:
	kfree(reply_body);
	if (sock) {
		sock_release(sock);
		fsi->native_sock = NULL;
	}
	fsi->native_connected = false;
	return ret;
}

void monofs_native_disconnect(struct monofs_fs_info *fsi)
{
	if (!fsi || !fsi->native_sock)
		return;

	sock_release(fsi->native_sock);
	fsi->native_sock = NULL;
	fsi->native_connected = false;
	fsi->native_session_id = 0;
}

int monofs_native_lookup(struct monofs_fs_info *fsi,
			 const u8 parent_object_id[MONOFS_NATIVE_OBJECT_ID_LEN],
			 const char *name,
			 struct monofs_native_lookup_reply *reply)
{
	struct monofs_native_frame reply_hdr;
	struct monofs_native_cursor cursor;
	u8 *body;
	u8 *reply_body = NULL;
	int ret;
	size_t name_len = strlen(name);
	size_t body_len = MONOFS_NATIVE_OBJECT_ID_LEN + 4 + name_len;

	memset(reply, 0, sizeof(*reply));

	body = kmalloc(body_len, GFP_KERNEL);
	if (!body)
		return -ENOMEM;
	cursor.data = body;
	cursor.len = body_len;
	cursor.off = 0;

	ret = monofs_native_put_bytes(&cursor, parent_object_id,
				      MONOFS_NATIVE_OBJECT_ID_LEN);
	if (!ret)
		ret = monofs_native_put_string(&cursor, name);
	if (ret)
		goto out;

	ret = monofs_native_roundtrip(fsi, MONOFS_NATIVE_OPCODE_LOOKUP,
					body, cursor.off, &reply_hdr, &reply_body);
	if (ret)
		goto out;

	{
		struct monofs_native_cursor response = {
			.data = reply_body,
			.len = le32_to_cpu(reply_hdr.body_len),
			.off = 0,
		};
		u8 found;

		ret = monofs_native_get_u8(&response, &found);
		if (!ret)
			ret = monofs_native_get_u32(&response, &reply->entry_ttl_ms);
		if (ret)
			goto out;
		reply->found = found != 0;
		if (reply->found) {
			ret = monofs_native_get_bytes(&response, reply->object_id,
						       MONOFS_NATIVE_OBJECT_ID_LEN);
			if (!ret)
				ret = monofs_native_get_attr(&response, &reply->attr);
		}
	}
out:
	kfree(body);
	kfree(reply_body);
	return ret;
}

int monofs_native_readdir(struct monofs_fs_info *fsi,
			  const u8 dir_object_id[MONOFS_NATIVE_OBJECT_ID_LEN],
			  u64 cookie,
			  u32 max_entries,
			  struct monofs_native_readdir_reply *reply)
{
	struct monofs_native_frame reply_hdr;
	struct monofs_native_cursor cursor;
	u8 *body;
	u8 *reply_body = NULL;
	int ret;
	size_t body_len = MONOFS_NATIVE_OBJECT_ID_LEN + 8 + 4 + 4;
	u32 count;
	u32 i;

	memset(reply, 0, sizeof(*reply));

	body = kmalloc(body_len, GFP_KERNEL);
	if (!body)
		return -ENOMEM;

	cursor.data = body;
	cursor.len = body_len;
	cursor.off = 0;

	ret = monofs_native_put_bytes(&cursor, dir_object_id,
				      MONOFS_NATIVE_OBJECT_ID_LEN);
	if (!ret)
		ret = monofs_native_put_u64(&cursor, cookie);
	if (!ret)
		ret = monofs_native_put_u32(&cursor, max_entries);
	if (!ret)
		ret = monofs_native_put_u32(&cursor, 0);
	if (ret)
		goto out;

	ret = monofs_native_roundtrip(fsi, MONOFS_NATIVE_OPCODE_READDIR,
					body, cursor.off, &reply_hdr, &reply_body);
	if (ret)
		goto out;

	{
		struct monofs_native_cursor response = {
			.data = reply_body,
			.len = le32_to_cpu(reply_hdr.body_len),
			.off = 0,
		};
		u8 eof;

		ret = monofs_native_get_u32(&response, &reply->dir_ttl_ms);
		if (!ret)
			ret = monofs_native_get_u64(&response, &reply->next_cookie);
		if (!ret)
			ret = monofs_native_get_u8(&response, &eof);
		if (!ret)
			ret = monofs_native_get_u32(&response, &count);
		if (ret)
			goto out;
		reply->eof = eof != 0;
		reply->entry_count = count;
		if (!count)
			goto out;

		reply->entries = kcalloc(count, sizeof(*reply->entries), GFP_KERNEL);
		if (!reply->entries) {
			ret = -ENOMEM;
			goto out;
		}

		for (i = 0; i < count; i++) {
			ret = monofs_native_get_string(&response, &reply->entries[i].name);
			if (!ret)
				ret = monofs_native_get_bytes(&response,
							       reply->entries[i].object_id,
							       MONOFS_NATIVE_OBJECT_ID_LEN);
			if (!ret)
				ret = monofs_native_get_u64(&response, &reply->entries[i].ino);
			if (!ret)
				ret = monofs_native_get_u32(&response, &reply->entries[i].mode);
			if (ret)
				goto out;
		}
	}

out:
	kfree(body);
	kfree(reply_body);
	if (ret)
		monofs_native_readdir_free(reply);
	return ret;
}

void monofs_native_readdir_free(struct monofs_native_readdir_reply *reply)
{
	u32 i;

	if (!reply || !reply->entries)
		return;

	for (i = 0; i < reply->entry_count; i++)
		kfree(reply->entries[i].name);
	kfree(reply->entries);
	reply->entries = NULL;
	reply->entry_count = 0;
}

int monofs_native_statfs(struct monofs_fs_info *fsi,
			 struct monofs_native_statfs_reply *reply)
{
	struct monofs_native_frame reply_hdr;
	struct monofs_native_cursor response;
	u8 *reply_body = NULL;
	int ret;

	memset(reply, 0, sizeof(*reply));

	ret = monofs_native_roundtrip(fsi, MONOFS_NATIVE_OPCODE_STATFS,
					NULL, 0, &reply_hdr, &reply_body);
	if (ret)
		goto out;

	response.data = reply_body;
	response.len = le32_to_cpu(reply_hdr.body_len);
	response.off = 0;

	ret = monofs_native_get_u64(&response, &reply->blocks);
	if (!ret)
		ret = monofs_native_get_u64(&response, &reply->bfree);
	if (!ret)
		ret = monofs_native_get_u64(&response, &reply->bavail);
	if (!ret)
		ret = monofs_native_get_u64(&response, &reply->files);
	if (!ret)
		ret = monofs_native_get_u64(&response, &reply->ffree);
	if (!ret)
		ret = monofs_native_get_u32(&response, &reply->bsize);
	if (!ret)
		ret = monofs_native_get_u32(&response, &reply->frsize);
	if (!ret)
		ret = monofs_native_get_u32(&response, &reply->name_len);
out:
	kfree(reply_body);
	return ret;
}

int monofs_native_ping(struct monofs_fs_info *fsi, u64 *generation)
{
	struct monofs_native_frame reply_hdr;
	u8 *reply_body = NULL;
	int ret;

	ret = monofs_native_roundtrip(fsi, MONOFS_NATIVE_OPCODE_PING,
				      NULL, 0, &reply_hdr, &reply_body);
	if (!ret && generation)
		*generation = le64_to_cpu(reply_hdr.generation);
	kfree(reply_body);
	return ret;
}

int monofs_native_open(struct monofs_fs_info *fsi,
		       const u8 object_id[MONOFS_NATIVE_OBJECT_ID_LEN],
		       u64 *handle_id,
		       struct monofs_native_attr *attr)
{
	struct monofs_native_frame reply_hdr;
	struct monofs_native_cursor cursor;
	u8 *body;
	u8 *reply_body = NULL;
	int ret;

	body = kmalloc(MONOFS_NATIVE_OBJECT_ID_LEN, GFP_KERNEL);
	if (!body)
		return -ENOMEM;

	cursor.data = body;
	cursor.len = MONOFS_NATIVE_OBJECT_ID_LEN;
	cursor.off = 0;
	ret = monofs_native_put_bytes(&cursor, object_id,
				      MONOFS_NATIVE_OBJECT_ID_LEN);
	if (ret)
		goto out;

	ret = monofs_native_roundtrip(fsi, MONOFS_NATIVE_OPCODE_OPEN_READ,
					body, cursor.off, &reply_hdr, &reply_body);
	if (ret)
		goto out;

	{
		struct monofs_native_cursor response = {
			.data = reply_body,
			.len = le32_to_cpu(reply_hdr.body_len),
			.off = 0,
		};
		u32 route_ttl_ms;

		ret = monofs_native_get_u64(&response, handle_id);
		if (!ret)
			ret = monofs_native_get_attr(&response, attr);
		if (!ret)
			ret = monofs_native_get_u32(&response, &route_ttl_ms);
		if (!ret)
			fsi->native_route_ttl_ms = route_ttl_ms;
	}
out:
	kfree(body);
	kfree(reply_body);
	return ret;
}

int monofs_native_read(struct monofs_fs_info *fsi,
		       u64 handle_id,
		       u64 offset,
		       u32 length,
		       void *data,
		       u32 *bytes_read,
		       bool *eof)
{
	struct monofs_native_frame reply_hdr;
	struct monofs_native_cursor cursor;
	u8 *body;
	u8 *reply_body = NULL;
	int ret;
	u32 payload_len;
	u8 eof_byte;

	body = kmalloc(8 + 8 + 4, GFP_KERNEL);
	if (!body)
		return -ENOMEM;

	cursor.data = body;
	cursor.len = 8 + 8 + 4;
	cursor.off = 0;

	ret = monofs_native_put_u64(&cursor, handle_id);
	if (!ret)
		ret = monofs_native_put_u64(&cursor, offset);
	if (!ret)
		ret = monofs_native_put_u32(&cursor, length);
	if (ret)
		goto out;

	ret = monofs_native_roundtrip(fsi, MONOFS_NATIVE_OPCODE_READ,
					body, cursor.off, &reply_hdr, &reply_body);
	if (ret)
		goto out;

	{
		struct monofs_native_cursor response = {
			.data = reply_body,
			.len = le32_to_cpu(reply_hdr.body_len),
			.off = 0,
		};

		ret = monofs_native_get_u8(&response, &eof_byte);
		if (!ret)
			ret = monofs_native_get_u32(&response, &payload_len);
		if (!ret && payload_len > length)
			ret = -EIO;
		if (!ret)
			ret = monofs_native_get_bytes(&response, data, payload_len);
		if (!ret) {
			*bytes_read = payload_len;
			*eof = eof_byte != 0;
		}
	}
out:
	kfree(body);
	kfree(reply_body);
	return ret;
}

int monofs_native_close(struct monofs_fs_info *fsi, u64 handle_id)
{
	struct monofs_native_frame reply_hdr;
	struct monofs_native_cursor cursor;
	u8 *body;
	u8 *reply_body = NULL;
	int ret;

	body = kmalloc(8, GFP_KERNEL);
	if (!body)
		return -ENOMEM;

	cursor.data = body;
	cursor.len = 8;
	cursor.off = 0;
	ret = monofs_native_put_u64(&cursor, handle_id);
	if (ret)
		goto out;

	ret = monofs_native_roundtrip(fsi, MONOFS_NATIVE_OPCODE_CLOSE,
					body, cursor.off, &reply_hdr, &reply_body);
out:
	kfree(body);
	kfree(reply_body);
	return ret;
}
