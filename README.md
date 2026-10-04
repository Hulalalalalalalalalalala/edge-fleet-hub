# envelopefile

当前版本提供：

- 命令行版本查询（`--version`）；
- `keygen` 子命令：生成一个 32 字节的二进制密钥文件，供后续文件加密
  功能使用（本版本暂不提供加密/解密操作）。

## 构建与运行

### 依赖与构建准备

`keygen` 的安全随机字节来自成熟密码库 **OpenSSL**（`libcrypto` 的
`RAND_bytes`，底层由操作系统的安全随机源 `/dev/urandom`/`getrandom(2)`
提供，不会基于时间、路径或普通伪随机数生成）。

构建前需安装 OpenSSL 开发文件：

```sh
# Debian / Ubuntu
sudo apt install libssl-dev

# Fedora / RHEL
sudo dnf install openssl-devel

# macOS（Homebrew）
brew install openssl
```

现有的构建方式保持不变：

```sh
cmake -S . -B build
cmake --build build
```

### 查询版本

```sh
./build/envelopefile --version
```

输出：

```text
envelopefile 0.1.0
```

### 生成密钥文件

必须用 `--output` 明确指定保存位置（路径不可为空），生成时不需要口令：

```sh
./build/envelopefile keygen --output ./my-envelope.key
```

成功时退出码为 0，标准输出显示完成提示及保存路径（不会打印密钥内容）：

```text
envelopefile: 32-byte key generated and saved to './my-envelope.key'
```

## 密钥文件说明

- 文件内容是 **恰好 32 字节的原始二进制数据**，不是可直接阅读的文本，
  没有换行、文件头或任何文本编码（如 Base64）。可以用
  `wc -c < my-envelope.key` 确认大小，用 `xxd my-envelope.key` 查看字节。
- 请妥善保管：**密钥丢失后无法重新生成相同的密钥**，用该密钥加密的数据
  将无法恢复；密钥泄露则加密失去意义。建议将文件保存在访问受控的位置，
  复制备份时同样注意保密。文件创建时权限即设为仅当前用户可读写（`0600`）。
- 若 `--output` 指定的路径已经存在（无论是普通文件、目录，还是符号链接
  ——包括指向不存在位置的悬空链接），程序都会拒绝生成，不会修改该路径
  或其链接目标。父目录不存在、目标不可写入、安全随机源失败、写入不完整
  或关闭失败时，会向标准错误报告原因并以非零退出码结束，且不会留下不完整
  的密钥文件。程序在内存中的密钥副本会在操作结束时清除。

## 命令行用法

```text
Usage:
  envelopefile --version
  envelopefile keygen --output <path>
```

缺少 `--output`、路径为空、重复指定 `--output` 或出现不支持的参数时，
报用法错误（退出码 2）且不生成任何文件。

## 回归测试

`tests/` 提供 `keygen` 的自动化回归保障，覆盖：成功生成（恰好 32 字节
原始数据、权限 0600——即使在宽松的 umask 下、路径含空格、密钥内容不进入
标准输出/标准错误/测试报告）、各类已存在目标的拒绝（普通文件、零长度
文件、目录、符号链接、悬空符号链接，且目标内容与权限保持原样），以及
创建后写入/同步/关闭失败时的报错与不完整文件清理（经 LD_PRELOAD 故障
注入模拟；部分写入后补齐仍应得到完整密钥）。

构建后通过 CTest 运行：

```sh
cmake --build build
ctest --test-dir build --output-on-failure
```

也可以直接运行测试驱动脚本：

```sh
python3 tests/run_keygen_regression.py \
    --binary build/envelopefile \
    --fault-inject build/libkeygen_fault_inject.so
```
