# cib — 本地容器镜像构建引擎

`cib` 在本机直接把「构建描述文件 + 构建上下文目录」构建成 OCI 镜像产物，
**不依赖任何守护进程，不访问网络**。同样的输入在任意机器、任意时间构建，
产出的镜像配置与清单逐字节一致。

## 快速开始

```sh
go build -o cib ./cmd/cib

./cib build \
  -f Containerfile \        # 构建描述文件
  -C ./context \            # 构建上下文目录
  -o ./out \                # 输出 OCI 布局目录
  --platform linux/amd64,linux/arm64 \
  --cache-dir .cib-cache
```

## 构建描述文件

指令集（大小写不敏感，`\` 续行，`#` 注释）：

| 指令 | 说明 |
|---|---|
| `FROM <ref> [AS name]` | `ref` 为 `scratch`、先前阶段名或 `oci:<本地OCI布局目录>`。每个阶段一条，必须是首条指令 |
| `COPY [--from=<stage>] [--chmod=<8进制>] <src...> <dest>` | 从构建上下文或先前阶段复制文件。多源时 `dest` 必须以 `/` 结尾 |
| `ENV K=V ...` | 环境变量（值可带引号） |
| `LABEL K=V ...` | 标签 |
| `WORKDIR <path>` | 工作目录，影响后续相对路径 COPY 的目标解析 |
| `CMD` / `ENTRYPOINT` | JSON 数组形式 `["/bin/app"]` 或 shell 形式（包装为 `/bin/sh -c`） |
| `USER <user>` | 运行用户 |
| `EXPOSE <port>[/proto] ...` | 暴露端口，缺省协议 `tcp` |

规则：

- 阶段只能引用**之前定义**的阶段（`FROM base`、`COPY --from=base`），
  引用不存在的阶段或路径会报出**带行号**的错误并终止构建，绝不静默跳过。
- 解析错误、未知指令、非法 JSON 数组、非法 `--chmod` 都会以
  `buildfile: line N: ...` 形式报出。

## 构建上下文与忽略规则

- 上下文根目录下的 `.cibignore`（可用 `Options.IgnoreFile` 改名）声明忽略规则，
  语法：`*`（段内通配）、`**`（跨段）、`/` 结尾仅匹配目录、前导 `!` 取消忽略、
  含 `/` 的模式锚定在根目录。被忽略的内容**不会**进入镜像，也不会参与缓存键计算。
- 每个文件以 `sha256` 内容摘要做变更检测；文件内容不变即视为不变，
  mtime、属主、遍历顺序均不影响结果。

## 产物结构（OCI Image Layout）

```
out/
├── oci-layout            # {"imageLayoutVersion":"1.0.0"}
├── index.json            # 镜像索引（多架构清单列表）
└── blobs/sha256/<hex>    # 层 tar、镜像配置、清单，全部按内容寻址
```

- `index.json` 的每个 `manifests[]` 条目带 `platform` 字段（`os`/`architecture`），
  运行时装载工具按平台选择对应清单；清单按平台字符串排序，保证字节稳定。
- 层为**未压缩 tar**（`application/vnd.oci.image.layer.v1.tar`），
  层摘要即 diffID。
- 产物可直接被支持 OCI 布局的运行时/工具（如 `skopeo copy oci:out ...`）装载。

## 摘要计算规则（可复现性）

固定配置下重复构建，所有摘要逐字节一致。保证手段：

1. **层 tar 归一化**：条目按路径字典序排列；uid/gid 恒为 0，uname/gname 为空；
   mtime 固定为 `--epoch`（默认 Unix 0）；仅保留权限位（setuid/sticky 等被剥离）。
2. **JSON 规范化**：配置/清单/索引按结构体字段序序列化，map 键排序，
   无多余空白，不转义 HTML。
3. **缓存链键**：
   `key₀ = sha256("cib/v1", os, arch, FROM 规范形式)`，
   每条 COPY：`keyᵢ = sha256(keyᵢ₋₁, COPY 规范形式, 输入内容摘要)`。
   输入内容摘要覆盖目标路径、权限位与每个源文件的内容摘要（或符号链接目标）。
   上游任一指令或输入文件变化，下游所有键全部失效。
4. **时间戳**：镜像配置与 history 的 `created` 一律为 `--epoch`。

## 层缓存

- 缓存目录（`--cache-dir`）跨构建、跨进程共享：
  - `blobs/sha256/<hex>`：内容寻址的层 blob；
  - `index/<key>`：链键 → 层摘要；
  - `locks/`、`tmp/`：锁与临时文件。
- 写入均为「临时文件 + fsync + rename」原子提交；并发读写由进程内互斥锁 +
  每键文件锁（flock）保护，读者只会看到完整的新旧条目，不会看到半写入条目。
- 索引项损坏或 blob 缺失时自动降级为缓存未命中并清理坏条目。
- 构建被取消或超时后，缓存保持一致；暂存目录随构建结束清理，
  输出目录通过「临时目录 + 原子换名」提交，失败不会留下半成品。

## 多架构

`--platform linux/amd64,linux/arm64,...` 一次构建产出多份清单并合成索引。
平台字符串必须是严格的 `os/arch`。使用 `FROM oci:<dir>` 基础镜像时，
若基础镜像缺少所请求的架构，构建会明确报错
（`base image ... has no manifest for platform ...`），绝不返回错误平台的内容。

## 错误与退出

所有失败路径都以非零退出码终止，错误信息可定位到阶段与指令行号：

```
cib: error: stage "final" line 5 (COPY): COPY source missing.txt: no such file in build context: missing.txt
cib: error: buildfile: line 1: FROM references unknown stage or image nosuchstage
```

超大单层由 `--max-layer-size`（默认 64 GiB）限制，超限即失败。

## CLI 配置项

| 参数 | 默认 | 说明 |
|---|---|---|
| `-f` | `Containerfile` | 构建描述文件路径 |
| `-C` | `.` | 构建上下文目录 |
| `-o` | （必填） | 输出 OCI 布局目录 |
| `--platform` | `linux/amd64` | 逗号分隔的目标平台列表 |
| `--cache-dir` | `.cib-cache` | 层缓存目录 |
| `--epoch` | `0` | 写入镜像的固定 Unix 时间戳 |
| `--max-layer-size` | 64 GiB | 单层大小上限（字节） |
| `--timeout` | 无 | 构建超时（如 `30s`），超时即取消并清理 |

## 库用法

```go
eng, err := engine.New(engine.Options{
    BuildfilePath: "Containerfile",
    ContextDir:    "./context",
    CacheDir:      ".cib-cache",
    OutputDir:     "./out",
    Platforms:     []string{"linux/amd64", "linux/arm64"},
    Epoch:         time.Unix(0, 0).UTC(),
})
res, err := eng.Build(ctx) // ctx 可随时取消
```

## 测试

```sh
go test ./...        # 单元 + 集成测试
go test -race ./...  # 并发构建竞态检测
```

覆盖：缓存命中/失效与跨构建复用、上游变更传导失效、多阶段依赖与跨阶段
COPY、多架构索引合成与缺架构报错、共享缓存并发构建、取消后的一致性与
恢复、忽略规则、逐字节可复现性、各类错误路径。
