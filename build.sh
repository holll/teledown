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
#   OUTPUT_NAME   输出文件名，默认 teledown
#   PYTHON        python 解释器，默认 python
#   KEEP_COMPILED 置 1 时保留 Cython 编译产物（*.so/*.pyd）
# ============================================================

# 项目根目录（兼容 Windows Git Bash 的正斜杠路径）
PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
VENV_DIR="$PROJECT_DIR/venv"
MAIN_FILE="main.py"
TOOLS_DIR="$PROJECT_DIR/tools"
OUTPUT_NAME="${OUTPUT_NAME:-teledown}"
PYTHON="${PYTHON:-python}"

# 识别操作系统，用于后续平台差异处理
UNAME_S="$(uname -s)"
case "$UNAME_S" in
    Linux*)       OS_NAME=linux ;;
    Darwin*)      OS_NAME=macos ;;
    MINGW*|MSYS*|CYGWIN*) OS_NAME=windows ;;
    *)            OS_NAME=linux ;;
esac
echo "[*] 检测到操作系统: $OS_NAME"

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
python -m pip install -r requirements.txt -r requirements_upload.txt
python -m pip install python-socks

# 文件类型识别库（magic）：Linux/macOS 用 python-magic，Windows 用 python-magic-bin
# 上传功能为可选，安装失败不阻塞打包
case "$OS_NAME" in
    windows)
        python -m pip install python-magic-bin==0.4.14 || echo "[!] python-magic-bin 安装失败，上传功能将不可用"
        ;;
    *)
        python -m pip install python-magic || echo "[!] python-magic 安装失败，上传功能将不可用"
        ;;
esac

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
    python_socks   # main.py 中按需导入的代理库
    cryptg         # telethon 运行时条件导入的加密库
    magic          # upload 文件类型识别（未安装时 PyInstaller 仅告警）
    pandas
    moviepy
)

HIDDEN_ARGS=()
for mod in "${HIDDEN_IMPORTS[@]}"; do
    HIDDEN_ARGS+=(--hidden-import "$mod")
done

python -m PyInstaller \
    --clean \
    --noconfirm \
    --onefile \
    --name "$OUTPUT_NAME" \
    "${HIDDEN_ARGS[@]}" \
    --collect-data demoji \
    --collect-submodules moviepy \
    "$MAIN_FILE"

# ------------------------------------------------------------
# 附带运行时配置模板（方便分发）
# ------------------------------------------------------------
for f in .env.example sign_tasks.example.json; do
    if [ -f "$PROJECT_DIR/$f" ]; then
        cp "$PROJECT_DIR/$f" dist/ 2>/dev/null || true
    fi
done

echo "================ 打包完成 ================"
echo "生成文件在 dist/$OUTPUT_NAME"
echo "运行 ./dist/$OUTPUT_NAME 启动程序"

# ------------------------------------------------------------
# 清理编译产物，恢复源码目录（PyInstaller 已把扩展打包进可执行文件）
# ------------------------------------------------------------
if [ "${KEEP_COMPILED:-0}" != "1" ]; then
    echo "[*] 清理 Cython 编译产物..."
    rm -f "$TOOLS_DIR"/*.so "$TOOLS_DIR"/*.pyd
    rm -rf "$PROJECT_DIR/build"
fi

deactivate 2>/dev/null || true