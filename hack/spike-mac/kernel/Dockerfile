# Native arm64 build image for apple/containerization's kernel. Apple's own
# image is Ubuntu 20.04, whose pahole (1.15) is too old to emit BTF for Linux
# 6.18, and Cilium 1.20 cannot load its programs without BTF.
FROM docker.io/library/ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      bc bison flex gcc make libc6-dev libelf-dev libssl-dev dwarves \
      python3 python-is-python3 xz-utils cpio kmod ca-certificates \
 && apt-get clean && rm -rf /var/lib/apt/lists/*
