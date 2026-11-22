#!/bin/sh
# 用无界面的 Chrome 截取预览页面：dev/shot.sh <地址路径> <输出文件> [宽度] [dark]
# 先在另一个终端执行 KILN_PREVIEW=1 pnpm dev --port 5391。
set -eu
path=$1
out=$2
width=${3:-1440}
scheme=""
if [ "${4:-}" = dark ]; then scheme="--blink-settings=preferredColorScheme=0"; fi
url="http://localhost:${PORT:-5391}$path"
win=$width
# Chrome 的窗口最窄约 500px：更窄的宽度放在 iframe 中截取
if [ "$width" -lt 500 ]; then
  src=$(printf %s "$path" | sed 's/%/%25/g; s/&/%26/g; s/?/%3F/g; s/=/%3D/g')
  url="http://localhost:${PORT:-5391}/dev/frame.html?w=$width&h=${HEIGHT:-1100}&src=$src"
  win=500
fi
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu --hide-scrollbars \
  --window-size="$win,${HEIGHT:-1100}" --virtual-time-budget=4000 $scheme \
  --screenshot="$out" "$url" >/dev/null 2>&1
echo "$out"
