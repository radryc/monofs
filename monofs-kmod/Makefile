# SPDX-License-Identifier: GPL-2.0-only

ifneq ($(KERNELRELEASE),)

obj-m := monofs.o
monofs-y := main.o fs_context.o super.o namespace.o native.o read.o

else

KDIR ?= /lib/modules/$(shell uname -r)/build

.PHONY: all clean help

all:
	@if [ ! -d "$(KDIR)" ]; then \
		echo "Error: kernel build directory '$(KDIR)' not found."; \
		echo "Install matching kernel headers or set KDIR=/path/to/kernel/build."; \
		exit 1; \
	fi
	$(MAKE) -C $(KDIR) M=$(CURDIR) modules

clean:
	@rm -rf *.o *.ko *.mod *.mod.c *.symvers modules.order Module.symvers .tmp_versions .*.cmd

help:
	@echo "Usage: make -C monofs-kmod [all|clean]"
	@echo "  all   Build the out-of-tree monofs kernel module"
	@echo "  clean Remove generated kernel module artifacts"
	@echo ""
	@echo "Override KDIR if your kernel headers live elsewhere:"
	@echo "  make -C monofs-kmod KDIR=/path/to/kernel/build all"

endif
