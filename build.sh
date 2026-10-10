#!/usr/bin/env bash
# ============================================================
#  LLMProxy build script (Linux / macOS)
#
#  用法:
#    ./build.sh                       编译当前系统 -> ./llmproxy
#    ./build.sh <os> [arch]           交叉编译,产物在 dist/llmproxy-<os>-<arch>[.exe]
#    ./build.sh all                   一次性编译全部支持的平台,并生成
#                                     dist/sha256_checksums.txt
#    ./build.sh -h | --help           显示帮助
#
#  支持的目标平台 (os/arch):
#    windows: 386  amd64  arm64
#    linux:   386  amd64  arm  arm64  mips  mipsle  mips64  mips64le
#    darwin:  amd64  arm64
#    freebsd: amd64  arm64
#    openbsd: amd64  arm64
#    android: arm64
# ============================================================
set -euo pipefail
cd "$(dirname "$0")"

# ---- 平台支持表 (GOOS/GOARCH) ----
ALL_TARGETS=(
  "windows/amd64"
  "windows/arm64"
  "windows/386"
  "linux/amd64"
  "linux/arm64"
  "linux/arm"
  "linux/386"
  "linux/mips"
  "linux/mipsle"
  "linux/mips64"
  "linux/mips64le"
  "darwin/amd64"
  "darwin/arm64"
  "freebsd/amd64"
  "freebsd/arm64"
  "openbsd/amd64"
  "openbsd/arm64"
  "android/arm64"
)

LDFLAGS_BASE="-s -w"
if [ -n "${VERSION:-}" ]; then
  LDFLAGS_BASE="$LDFLAGS_BASE -X main.buildVersion=$VERSION"
fi

TARGET_OS="${1:-}"
TARGET_ARCH="${2:-}"

show_help() {
  cat <<'USAGE'
LLMProxy 编译脚本 (Linux / macOS)
用法:
  ./build.sh                       编译当前系统 -> ./llmproxy
  ./build.sh <os> [arch]           交叉编译,产物在 dist/llmproxy-<os>-<arch>[.exe]
  ./build.sh all                   一次性编译全部平台,并生成 dist/sha256_checksums.txt
  ./build.sh -h | --help           显示帮助
示例:
  ./build.sh                       编译当前平台
  ./build.sh linux                 交叉编译 linux/amd64
  ./build.sh linux arm64           交叉编译 linux/arm64
  ./build.sh windows amd64         交叉编译 Windows amd64
  ./build.sh all                   一次性构建全部平台(产物在 dist/)
支持的目标 (os/arch):
  windows: 386  amd64  arm64
  linux:   386  amd64  arm  arm64  mips  mipsle  mips64  mips64le
  darwin:  amd64  arm64
  freebsd: amd64  arm64
  openbsd: amd64  arm64
  android: arm64
可选环境变量:
  VERSION=1.2.3 ./build.sh         注入版本号(默认使用源码 main.go 的 buildVersion)
  GOPROXY=... ./build.sh           覆盖默认代理
USAGE
}

case "$TARGET_OS" in
  -h|--help) show_help; exit 0 ;;
  "") ;;
  all) ;;
  *) [ -z "$TARGET_ARCH" ] && TARGET_ARCH="amd64" ;;
esac

# ---- 检查 go ----
if ! command -v go >/dev/null 2>&1; then
  echo "[错误] 未找到 go 命令,请先安装 Go 1.26.5+ 并加入 PATH"
  exit 1
fi

# ---- GOPROXY 默认镜像 ----
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

filesize() { du -h "$1" 2>/dev/null | cut -f1; }

# ---- 构建单个目标 (失败返回非零,避免 bash 5.3 在 if 条件下抑制 set -e 的坑) ----
build_one() {
  local os="$1" arch="$2"
  local ext=""
  [ "$os" = "windows" ] && ext=".exe"
  local out="dist/llmproxy-${os}-${arch}${ext}"
  mkdir -p dist
  if CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
       go build -ldflags "$LDFLAGS_BASE" -o "$out" . 2>/dev/null; then
    local size
    size=$(filesize "$out")
    echo "[成功] $out (${size:-?})"
    return 0
  else
    local rc=$?
    rm -f "$out" 2>/dev/null || true
    return $rc
  fi
}

# ==================== all 模式 ====================
if [ "$TARGET_OS" = "all" ]; then
  total=${#ALL_TARGETS[@]}
  echo "==> 一次性构建全部平台 (${total} 个组合)"
  mkdir -p dist
  rm -f dist/llmproxy-* dist/sha256_checksums.txt 2>/dev/null || true
  skipped=()
  i=0
  for t in "${ALL_TARGETS[@]}"; do
    i=$((i+1))
    os="${t%%/*}"; arch="${t##*/}"
    echo ""
    echo "==> [${i}/${total}] ${os}/${arch}"
    if build_one "$os" "$arch" 2>/dev/null; then
      :
    else
      echo "[跳过] ${os}/${arch} (构建失败,当前 Go 工具链或第三方依赖不支持)"
      skipped+=("${os}/${arch}")
    fi
  done
  echo ""
  echo "==> 生成 dist/sha256_checksums.txt"
  if ls dist/llmproxy-* 1>/dev/null 2>&1; then
    # 注意: ( cd dist && sha256sum ) 在子 shell 内 cd 不会影响外围 `>` 重定向,
    # 所以这里显式 `cd dist ...; ... > sha256_checksums.txt`。
    ( cd dist && sha256sum llmproxy-* 2>/dev/null | sort -k2 ) > dist/sha256_checksums.txt
    cat dist/sha256_checksums.txt
  else
    : > dist/sha256_checksums.txt
    echo "  (没有任何产物)"
  fi
  echo ""
  if [ ${#skipped[@]} -gt 0 ]; then
    echo "[完成] ${#skipped[@]} 个目标被跳过: ${skipped[*]}"
    echo "       已构建产物及校验见上"
  else
    echo "[完成] 全部 ${total} 个组合构建成功"
  fi
  exit 0
fi

# ==================== 当前平台 ====================
if [ -z "$TARGET_OS" ]; then
  # 清掉外部可能设置的交叉编译变量,确保编译当前平台
  unset GOOS GOARCH
  export CGO_ENABLED=0
  go build -ldflags "$LDFLAGS_BASE" -o llmproxy .
  echo "[成功] ./llmproxy ($(filesize llmproxy))"
  exit 0
fi

# ==================== 交叉编译指定平台 ====================
found=0
for t in "${ALL_TARGETS[@]}"; do
  if [ "$t" = "${TARGET_OS}/${TARGET_ARCH}" ]; then
    found=1; break
  fi
done
if [ "$found" != "1" ]; then
  echo "[错误] 不支持的组合: ${TARGET_OS}/${TARGET_ARCH}"
  echo "       请运行 ./build.sh --help 查看支持列表"
  exit 1
fi

build_one "$TARGET_OS" "$TARGET_ARCH"
