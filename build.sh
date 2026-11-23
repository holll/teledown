#!/bin/bash
set -e

# ============================================================
# teledown 一键打包脚本
#   1. 自动创建/复用虚拟环境并安装依赖
#   2. 用 Cython 把 tools/*.py 编译为 C 扩展（保护源码）
#   3. 用 PyInstaller 打包为单文件可执行程序
#
# 兼容：Linux / macOS / Windows(Git Bash / MSYS2)
#
# 可通过环境变量覆盖默认配置：
#   OUTPUT_NAME   输出文件名前缀，默认 teledown（最终名：前缀-平台-架构）
#   PYTHON        python 解释器，默认 python
#   KEEP_COMPILED 置 1 时保留 Cython 编译产物（*.so/*.pyd）
#   UPLOAD_SUPPORT 置 0 时构建精简版（不打包 moviepy/Pillow/python-magic，无 upload 功能）
# ============================================================

# 项目根目录（兼容 Windows Git Bash 的正斜杠路径）
PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
VENV_DIR="$PROJECT_DIR/venv"
MAIN_FILE="main.py"
TOOLS_DIR="$PROJECT_DIR/tools"
OUTPUT_NAME="${OUTPUT_NAME:-teledown}"
PYTHON="${PYTHON:-python}"

# 上传功能开关：1=全功能版（含 upload），0=精简版（无 upload，体积更小）
UPLOAD_SUPPORT="${UPLOAD_SUPPORT:-1}"
if [ "$UPLOAD_SUPPORT" = "1" ]; then
    echo "[*] 构建模式: 全功能版（含 upload）"
else
    echo "[*] 构建模式: 精简版（不含 upload）"
fi

# 识别操作系统，用于后续平台差异处理
UNAME_S="$(uname -s)"
case "$UNAME_S" in
    Linux*)       OS_NAME=linux ;;
    Darwin*)      OS_NAME=macos ;;
    MINGW*|MSYS*|CYGWIN*) OS_NAME=windows ;;
    *)            OS_NAME=linux ;;
esac
echo "[*] 检测到操作系统: $OS_NAME"

# 识别架构
MACHINE="$(uname -m)"
case "$MACHINE" in
    x86_64|amd64)  ARCH="x86_64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)             ARCH="$MACHINE" ;;
esac
echo "[*] 检测到架构: $ARCH"

# 产物名 = 程序名-平台-架构（Windows 下 PyInstaller 会自动追加 .exe）
FINAL_NAME="${OUTPUT_NAME}-${OS_NAME}-${ARCH}"

cd "$PROJECT_DIR"

# ------------------------------------------------------------
# 虚拟环境
# ------------------------------------------------------------
if [ -d "$VENV_DIR" ]; then
    echo "[*] 使用已有虚拟环境: $VENV_DIR"
else
    echo "[*] 创建虚拟环境: $VENV_DIR"
    "$PYTHON" -m venv "$VENV_DIR"
fi

# Windows 虚拟环境下激活脚本在 Scripts/ 下，Linux/macOS 在 bin/ 下
if [ -f "$VENV_DIR/Scripts/activate" ]; then
    # shellcheck disable=SC1091
    source "$VENV_DIR/Scripts/activate"
else
    # shellcheck disable=SC1091
    source "$VENV_DIR/bin/activate"
fi

# ------------------------------------------------------------
# 安装依赖
# ------------------------------------------------------------
echo "[*] 安装构建依赖..."
python -m pip install --upgrade pip
python -m pip install "setuptools<81" wheel cython pyinstaller

echo "[*] 安装项目运行依赖..."
python -m pip install -r requirements.txt
python -m pip install python-socks

# 上传相关依赖（moviepy / Pillow / python-magic）仅在 UPLOAD_SUPPORT=1 时安装
if [ "$UPLOAD_SUPPORT" = "1" ]; then
    python -m pip install -r requirements_upload.txt

    # 文件类型识别库（magic）：Linux/macOS 用 python-magic，Windows 用 python-magic-bin
    # 全功能版必须能加载 libmagic，不能把不可上传的产物标为全功能。
    case "$OS_NAME" in
        windows)
            python -m pip install python-magic-bin==0.4.14
            ;;
        *)
            python -m pip install python-magic
            ;;
    esac
    if [ "$OS_NAME" = "linux" ]; then
        MAGIC_DATABASE=''
        for candidate in /usr/share/misc/magic.mgc /usr/share/file/magic.mgc /usr/lib/file/magic.mgc; do
            if [ -f "$candidate" ] && [ -r "$candidate" ]; then
                MAGIC_DATABASE="$candidate"
                break
            fi
        done
        if [ -z "$MAGIC_DATABASE" ]; then
            echo '[!] 找不到 magic.mgc，请安装与 libmagic 匹配的规则库（Debian/Ubuntu：libmagic-mgc）' >&2
            exit 1
        fi
        python - "$MAGIC_DATABASE" <<'PY'
import sys
import magic

rules = magic.Magic(mime=True, magic_file=sys.argv[1])
mime = rules.from_buffer(b'GIF89a\x01\x00\x01\x00\x00\x00\x00')
if mime != 'image/gif':
    sys.exit(f'构建机的 libmagic 未能使用 {sys.argv[1]} 识别 GIF（{mime}）')
PY
    else
        python -c 'import magic; magic.Magic(mime=True).from_buffer(b"teledown")'
    fi
else
    echo "[*] 跳过上传相关依赖（moviepy / Pillow / python-magic）"
fi

# 验证 pkg_resources（旧版 PyInstaller/altgraph 依赖）
python - <<'EOF'
import pkg_resources
print("pkg_resources ok")
EOF

# ------------------------------------------------------------
# 用 Cython 编译 tools 下的模块（__init__.py 除外）
# ------------------------------------------------------------
echo "[*] 编译 tools 下的 Python 模块为 C 扩展..."
PY_FILES=$(find "$TOOLS_DIR" -maxdepth 1 -name "*.py" ! -name "__init__.py" 2>/dev/null || true)

for module_path in $PY_FILES; do
    MODULE_NAME="$(basename "$module_path")"
    echo "[*] 编译 $module_path ..."

    TMP_SETUP="$PROJECT_DIR/setup_temp.py"
    cat > "$TMP_SETUP" <<EOF
from setuptools import setup
from Cython.Build import cythonize

setup(
    ext_modules=cythonize(
        ["tools/$MODULE_NAME"],
        compiler_directives={'language_level': "3"}
    ),
)
EOF

    # 编译失败（如 Windows 缺少 C 编译器）时回退为纯 Python 打包
    set +e
    python "$TMP_SETUP" build_ext --inplace
    ret=$?
    set -e
    rm -rf build "$TMP_SETUP"

    if [ "$ret" -ne 0 ]; then
        echo "[!] Cython 编译失败，回退为纯 Python 打包（tools 源码将以字节码形式打包）"
        break
    fi
done

# ------------------------------------------------------------
# 清理旧产物
# ------------------------------------------------------------
rm -rf dist

# ------------------------------------------------------------
# PyInstaller 打包
# ------------------------------------------------------------
echo "[*] 开始使用 PyInstaller 打包..."

# 隐藏导入：函数内/运行时才 import 的模块，PyInstaller 静态分析可能遗漏
HIDDEN_IMPORTS=(
    csv            # tools.tool 经 Cython 编译后，PyInstaller 无法扫描其导入
    python_socks   # main.py 中按需导入的代理库
    cryptg         # telethon 运行时条件导入的加密库
)
if [ "$UPLOAD_SUPPORT" = "1" ]; then
    HIDDEN_IMPORTS+=(
        magic      # upload 文件类型识别（未安装时 PyInstaller 仅告警）
        moviepy    # upload 视频缩略图/时长
    )
fi

HIDDEN_ARGS=()
for mod in "${HIDDEN_IMPORTS[@]}"; do
    HIDDEN_ARGS+=(--hidden-import "$mod")
done

COLLECT_ARGS=(--collect-data demoji)
# CLI 不使用 Tk，避免 PyInstaller 带入 Tcl/Tk 运行时。
EXCLUDE_ARGS=(--exclude-module tkinter --exclude-module _tkinter)
if [ "$UPLOAD_SUPPORT" = "1" ]; then
    COLLECT_ARGS+=(--collect-submodules moviepy)
    if [ "$OS_NAME" = "linux" ]; then
        COLLECT_ARGS+=(
            --add-data "$MAGIC_DATABASE:teledown-magic"
            --add-data "$PROJECT_DIR/packaging/COPYING.libmagic:teledown-magic"
        )
    fi
else
    # 精简版：显式排除上传相关依赖（含 moviepy 依赖的 imageio/ffmpeg 二进制）
    EXCLUDE_ARGS+=(
        --exclude-module moviepy
        --exclude-module imageio
        --exclude-module imageio_ffmpeg
        --exclude-module PIL
        --exclude-module magic
    )
fi

python -m PyInstaller \
    --clean \
    --noconfirm \
    --onefile \
    --python-option 'X utf8' \
    --name "$FINAL_NAME" \
    --additional-hooks-dir "$PROJECT_DIR/packaging/hooks" \
    "${HIDDEN_ARGS[@]}" \
    "${EXCLUDE_ARGS[@]}" \
    "${COLLECT_ARGS[@]}" \
    "$MAIN_FILE"

BINARY_PATH="dist/$FINAL_NAME"
if [ "$OS_NAME" = "windows" ]; then
    BINARY_PATH+=.exe
fi
python - "$BINARY_PATH" "$OS_NAME" "$UPLOAD_SUPPORT" <<'PY'
import re
import sys
from PyInstaller.archive.readers import CArchiveReader

archive = CArchiveReader(sys.argv[1])
for entry in archive.toc:
    path = entry.replace('\\', '/').lower()
    if '/imageio_ffmpeg/binaries/' in '/' + path or re.fullmatch(
        r'(?:ffmpeg|ffprobe)(?:-[^.]+(?:\.[^.]+)*)?(?:\.exe)?', path.rsplit('/', 1)[-1]
    ):
        sys.exit(f'构建意外内置 FFmpeg 资源：{entry}')

if sys.argv[2] == 'linux':
    rules = 'teledown-magic/magic.mgc'
    notice = 'teledown-magic/COPYING.libmagic'
    has_libmagic = any(re.fullmatch(r'libmagic\.so(?:\..+)?', entry) for entry in archive.toc)
    if sys.argv[3] == '1' and (not has_libmagic or rules not in archive.toc or notice not in archive.toc):
        sys.exit('Linux 全功能版缺少 libmagic、magic.mgc 或版权声明')
    if sys.argv[3] != '1' and (has_libmagic or rules in archive.toc or notice in archive.toc):
        sys.exit('Linux 精简版意外包含 libmagic 资源')
PY

# ------------------------------------------------------------
# 附带运行时配置文件（方便分发/开箱即用）
# ------------------------------------------------------------
# 始终附带配置模板
if [ -f "$PROJECT_DIR/.env.example" ]; then
    cp "$PROJECT_DIR/.env.example" dist/.env.example
fi

# 本地构建时若存在真实 .env，一并复制为 dist/.env 便于直接运行
# 注意：.env 含 API 凭据，勿将 dist 对外分发
if [ -f "$PROJECT_DIR/.env" ]; then
    cp "$PROJECT_DIR/.env" dist/.env
    echo "[!] 已复制本地 .env 到 dist/.env（含凭据，仅供个人使用，勿对外分发）"
fi

if [ -f "$PROJECT_DIR/sign_tasks.example.json" ]; then
    cp "$PROJECT_DIR/sign_tasks.example.json" dist/sign_tasks.example.json
fi

echo "================ 打包完成 ================"
echo "生成文件在 dist/$FINAL_NAME"
echo "运行 ./dist/$FINAL_NAME 启动程序"

# ------------------------------------------------------------
# 清理编译产物，恢复源码目录（PyInstaller 已把扩展打包进可执行文件）
# ------------------------------------------------------------
if [ "${KEEP_COMPILED:-0}" != "1" ]; then
    echo "[*] 清理 Cython 编译产物..."
    rm -f "$TOOLS_DIR"/*.so "$TOOLS_DIR"/*.pyd
    rm -rf "$PROJECT_DIR/build"
fi

deactivate 2>/dev/null || true