// SPDX-License-Identifier: GPL-2.0-only

#include <linux/fs.h>
#include <linux/init.h>
#include <linux/module.h>

#include "monofs.h"

struct file_system_type monofs_fs_type = {
	.owner		= THIS_MODULE,
	.name		= MONOFS_NAME,
	.init_fs_context = monofs_init_fs_context,
	.parameters	= monofs_fs_parameters,
	.kill_sb	= monofs_kill_sb,
	.fs_flags	= FS_USERNS_MOUNT,
};

static int __init monofs_init(void)
{
	return register_filesystem(&monofs_fs_type);
}

static void __exit monofs_exit(void)
{
	unregister_filesystem(&monofs_fs_type);
}

module_init(monofs_init);
module_exit(monofs_exit);

MODULE_AUTHOR("MonoFS contributors");
MODULE_DESCRIPTION("MonoFS native kernel filesystem scaffold");
MODULE_LICENSE("GPL");
MODULE_ALIAS_FS(MONOFS_NAME);
