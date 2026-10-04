# envelopefile

命令行文件加密工具。当前版本提供：

- `--version`：版本查询
- `keygen`：生成 32 字节（256 位）随机密钥文件，供后续文件加密功能使用

## 构建与运行

### 依赖

- C++20 编译器（如 GCC 12+）
- CMake 3.20 或更高版本
- [OpenSSL](https://www.openssl.org/) 3.0 及以上版本（仅使用其 `libcrypto`
  中的安全随机数接口 `RAND_bytes`，不依赖 `libssl`）

在 Debian/Ubuntu 上安装构建依赖：

```sh
sudo apt-get install build-essential cmake libssl-dev
```

密钥的随机字节来自 OpenSSL 的安全随机来源（最终由操作系统的 CSPRNG
提供，如 Linux 的 `getrandom(2)`），不会使用时间、文件路径或普通伪随机数
生成。

### 构建

现有的构建方式保持不变：

```sh
cmake -S . -B build
cmake --build build
```

## 使用

### 查询版本

```sh
./build/envelopefile --version
```

输出：

```text
envelopefile 0.1.0
```

### 生成密钥文件

```sh
./build/envelopefile keygen --output ./master.key
```

成功时退出码为 0，标准输出显示完成提示和保存路径（不会显示密钥内容）：

```text
Key generated and saved to: ./master.key
```

生成密钥**不需要口令**，但必须通过 `--output` 明确给出保存位置。

生成的密钥文件特征：

- 内容为**恰好 32 字节的原始二进制数据**，没有换行、文件头或任何文本编码。
  它不是可以直接阅读的文本，用文本编辑器打开会显示为乱码；可用
  `xxd ./master.key` 以十六进制方式查看。
- 文件权限为仅当前用户可读写（POSIX 系统上为 `0600`），并且从创建的一刻起
  就是该权限。

#### 安全注意事项

- **妥善保管密钥文件**：它等同于文件加密的钥匙，建议存放在只有本人可访问的
  位置，并按需离线备份。任何获得该文件的人都可以解密用它加密的内容。
- **丢失后无法找回**：密钥由安全随机数一次性生成，系统不保存任何副本。密钥
  文件一旦丢失或损坏，无法重新生成相同的密钥，用它加密的数据将无法解密。
- 密钥内容不会出现在任何提示、错误信息或日志中；程序退出前会清除自身内存中
  的密钥副本。

#### 已有文件保护

`--output` 指定的路径如果已经存在，程序将拒绝生成，无论它是普通文件、目录
还是符号链接（包括指向不存在位置的悬空符号链接），也不会改动该路径或链接
所指向的文件。例如：

```text
$ ./build/envelopefile keygen --output ./master.key
Error: path already exists, refusing to overwrite: ./master.key
```

父目录不存在或目标位置不可写入时，会在标准错误中明确报告保存失败，程序不会
自动创建目录，也不会改用其他路径。

#### 退出码

| 退出码 | 含义                                                       |
| ------ | ---------------------------------------------------------- |
| `0`    | 密钥已成功生成并完整写入                                   |
| `1`    | 运行失败（随机源失败、路径已存在、无法写入、写入不完整等） |
| `2`    | 用法错误（缺少 `--output`、路径为空、参数重复或不支持的参数） |

用法示例：

```sh
# 缺少 --output
./build/envelopefile keygen                 # 退出码 2

# 路径为空
./build/envelopefile keygen --output ""     # 退出码 2

# 重复指定输出路径
./build/envelopefile keygen --output a.key --output b.key   # 退出码 2

# 不支持的参数
./build/envelopefile keygen --rotate        # 退出码 2
```

写入过程中发生任何失败（随机来源失败、写入不完整、文件落盘或关闭失败），
程序都会报告错误、删除未完成的目标文件并以非零退出码结束，不会留下不完整的
密钥文件，也不会报告成功。

本版本仅提供密钥文件生成功能，文件加密与解密操作将在后续版本提供。
