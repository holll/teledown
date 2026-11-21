# Override the default PyInstaller hook, which bundles the wheel's FFmpeg.
# Keep the Python module available to MoviePy/ImageIO, but no executable data.
datas = []
