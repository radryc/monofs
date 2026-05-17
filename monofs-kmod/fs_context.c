// SPDX-License-Identifier: GPL-2.0-only

#include <linux/errno.h>
#include <linux/fs_context.h>
#include <linux/fs_parser.h>
#include <linux/slab.h>
#include <linux/string.h>

#include "monofs.h"

enum monofs_param {
	Opt_gateway,
	Opt_auth_token,
	Opt_seed_paths,
	Opt_attr_ttl_ms,
	Opt_overlay_writes,
	Opt_cluster_version,
	Opt_debug,
};

const struct fs_parameter_spec monofs_fs_parameters[] = {
	fsparam_string("gateway", Opt_gateway),
	fsparam_string("auth_token", Opt_auth_token),
	fsparam_string("seed_paths", Opt_seed_paths),
	fsparam_u32("attr_ttl_ms", Opt_attr_ttl_ms),
	fsparam_flag("overlay_writes", Opt_overlay_writes),
	fsparam_u64("cluster_version", Opt_cluster_version),
	fsparam_flag("debug", Opt_debug),
	{}
};

static int monofs_append_seed_path(struct monofs_fs_info *fsi, const char *path)
{
	char **new_seed_paths;
	char *copy;
	u32 index;

	copy = kstrdup(path, GFP_KERNEL);
	if (!copy)
		return -ENOMEM;

	new_seed_paths = krealloc(fsi->mount_opts.seed_paths,
				  sizeof(*new_seed_paths) *
					  (fsi->mount_opts.seed_path_count + 1),
				  GFP_KERNEL);
	if (!new_seed_paths) {
		kfree(copy);
		return -ENOMEM;
	}

	index = fsi->mount_opts.seed_path_count;
	fsi->mount_opts.seed_paths = new_seed_paths;
	fsi->mount_opts.seed_paths[index] = copy;
	fsi->mount_opts.seed_path_count++;

	return 0;
}

static int monofs_parse_seed_paths(struct monofs_fs_info *fsi, char *value)
{
	char *cursor = value;

	while (cursor) {
		char *item = strsep(&cursor, ",");
		size_t len;
		int ret;

		if (!item)
			break;

		item = strim(item);
		while (*item == '/')
			item++;
		if (*item == '\0')
			continue;
		len = strlen(item);
		while (len > 0 && item[len - 1] == '/') {
			item[len - 1] = '\0';
			len--;
		}
		if (*item == '\0')
			continue;

		ret = monofs_append_seed_path(fsi, item);
		if (ret)
			return ret;
	}

	return 0;
}

static int monofs_parse_param(struct fs_context *fc, struct fs_parameter *param)
{
	struct monofs_fs_info *fsi = fc->s_fs_info;
	struct fs_parse_result result;
	char *value;
	int opt;

	opt = fs_parse(fc, monofs_fs_parameters, param, &result);
	if (opt == -ENOPARAM)
		return vfs_parse_fs_param_source(fc, param);
	if (opt < 0)
		return opt;

	switch (opt) {
	case Opt_gateway:
		value = kstrdup(param->string, GFP_KERNEL);
		if (!value)
			return -ENOMEM;
		kfree(fsi->mount_opts.gateway);
		fsi->mount_opts.gateway = value;
		break;
	case Opt_auth_token:
		value = kstrdup(param->string, GFP_KERNEL);
		if (!value)
			return -ENOMEM;
		kfree(fsi->mount_opts.auth_token);
		fsi->mount_opts.auth_token = value;
		break;
	case Opt_seed_paths:
		value = kstrdup(param->string, GFP_KERNEL);
		if (!value)
			return -ENOMEM;
		opt = monofs_parse_seed_paths(fsi, value);
		kfree(value);
		if (opt)
			return opt;
		break;
	case Opt_attr_ttl_ms:
		fsi->mount_opts.attr_ttl_ms = result.uint_32;
		break;
	case Opt_overlay_writes:
		fsi->mount_opts.overlay_writes = true;
		break;
	case Opt_cluster_version:
		fsi->cluster_version = result.uint_64;
		break;
	case Opt_debug:
		fsi->mount_opts.debug = true;
		break;
	default:
		return invalfc(fc, "Unsupported parameter '%s'", param->key);
	}

	return 0;
}

static int monofs_get_tree(struct fs_context *fc)
{
	struct monofs_fs_info *fsi = fc->s_fs_info;

	if (!fsi->mount_opts.gateway && fc->source) {
		fsi->mount_opts.gateway = kstrdup(fc->source, GFP_KERNEL);
		if (!fsi->mount_opts.gateway)
			return -ENOMEM;
	}

	return get_tree_nodev(fc, monofs_fill_super);
}

static void monofs_free_fc(struct fs_context *fc)
{
	struct monofs_fs_info *fsi = fc->s_fs_info;

	if (!fsi)
		return;

	monofs_free_fs_info(fsi);
	kfree(fsi);
}

static const struct fs_context_operations monofs_context_ops = {
	.free		= monofs_free_fc,
	.parse_param	= monofs_parse_param,
	.get_tree	= monofs_get_tree,
};

int monofs_init_fs_context(struct fs_context *fc)
{
	struct monofs_fs_info *fsi;

	fsi = kzalloc(sizeof(*fsi), GFP_KERNEL);
	if (!fsi)
		return -ENOMEM;

	fsi->mount_opts.attr_ttl_ms = MONOFS_DEFAULT_ATTR_TTL_MS;
	fsi->mount_opts.overlay_writes = true;
	fc->s_fs_info = fsi;
	fc->ops = &monofs_context_ops;

	return 0;
}
