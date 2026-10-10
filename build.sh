#!/usr/bin/env bash
# ============================================================
#  LLMProxy 编译脚本（Linux / macOS）
#
#  用法:
#    ./build.sh                    编译当前系统，输出 ./llmproxy
#    ./build.sh linux              交叉编译 linux/amd64
#    ./build.sh linux arm64        交叉编译 linux/arm64
#    ./build.sh darwin arm64       交叉编译 macOS (Apple Silicon)
#    ./build.sh windows amd64      交叉编译 Windows
#
#  交叉编译产物输出到 dist/ 目录，带平台后缀。
# ============================================================
set -euo pipefail
cd "$(dirname "$0")"

TARGET_OS="${1:-}"
TARGET_ARCH="${2:-}"
LDFLAGS="-s -w"

if [ -n "${VERSION:-}" ]; then
  LDFLAGS="$LDFLAGS -X main.buildVersion=$VERSION"
fi

if [ "$TARGET_OS" = "-h" ] || [ "$TARGET_OS" = "--help" ]; then
  cat <<'USAGE'
LLMProxy 编译脚本（Linux / macOS）：
用法:
  ./build.sh                  编译当前系统，输出 ./llmproxy
  ./build.sh <os> [arch]      交叉编译指定平台，输出到 dist/
示例:
  ./build.sh linux            交叉编译 linux/amd64
  ./build.sh linux arm64      交叉编译 linux/arm64
  ./build.sh darwin arm64     交叉编译 macOS (Apple Silicon)
  ./build.sh windows amd64    交叉编译 Windows amd64
支持的目标:
  windows: 386  amd64  arm64
  linux:   386  amd64  arm  arm64
  darwin:  amd64  arm64
可选环境变量:
  VERSION=1.2.3 ./build.sh    注入版本号（默认使用源码 main.go 中的 buildVersion）
  GOPROXY=... ./build.sh      覆盖默认代理
USAGE
  exit 0
fi

# ---- 检查 go ----
if ! command -v go >/dev/null 2>&1; then
  echo "[错误] 未找到 go 命令，请先安装 Go 1.22+ 并加入 PATH"
  exit 1
fi

# ---- GOPROXY 未设置时默认国内镜像 ----
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

filesize() {
  du -h "$1" 2>/dev/null | cut -f1
}

if [ -z "$TARGET_OS" ]; then
  # ==================== 编译当前系统 ====================
  # 清掉外部可能设置的交叉编译变量，确保编译当前平台
  unset GOOS GOARCH
  export CGO_ENABLED=0
  go build -ldflags "$LDFLAGS" -o llmproxy .
  echo "[成功] ./llmproxy ($(filesize llmproxy))"
  exit 0
fi

# ==================== 交叉编译指定平台 ====================
if [ -z "$TARGET_ARCH" ]; then
  TARGET_ARCH=amd64
fi

# ---- 校验目标系统 ----
case "$TARGET_OS" in
  windows) VALID_ARCHS="386 amd64 arm64" ;;
  linux)   VALID_ARCHS="386 amd64 arm arm64" ;;
  darwin)  VALID_ARCHS="amd64 arm64" ;;
  *)
    echo "[错误] 不支持的目标系统: $TARGET_OS"
    echo "       支持: windows / linux / darwin"
    exit 1
    ;;
esac

# ---- 校验 系统/架构 组合 ----
COMBO_OK=0
for a in $VALID_ARCHS; do
  if [ "$TARGET_ARCH" = "$a" ]; then
    COMBO_OK=1
  fi
done
if [ "$COMBO_OK" != "1" ]; then
  echo "[错误] 不支持的组合: $TARGET_OS/$TARGET_ARCH"
  echo "       $TARGET_OS 支持: $VALID_ARCHS"
  exit 1
fi

EXT=""
if [ "$TARGET_OS" = "windows" ]; then
  EXT=".exe"
fi
OUT="dist/llmproxy-$TARGET_OS-$TARGET_ARCH$EXT"

mkdir -p dist
CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" go build -ldflags "$LDFLAGS" -o "$OUT" .
echo "[成功] $OUT ($(filesize "$OUT"))"
