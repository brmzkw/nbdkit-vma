run:
	docker build -t test-proxmox .
	docker run --rm -ti --init -v .:/app --device /dev/fuse --cap-add SYS_ADMIN test-proxmox bash