#!/bin/bash
# piknik -watch 回归测试：回声抑制、双向阻断、图片同步
# 关键：每个用例都从确定的基线开始（先清空剪贴板和服务器内容），
# 用唯一 token，并轮询等待而不是固定 sleep。
set -u
BIN=${PIKNIK:-./piknik}
SRV=${PIKNIK_TEST_SERVER:-127.0.0.1:8077}   # 独立端口，避免和真实部署冲突
DIR="$(mktemp -d)"
BIN="$(readlink -f "$BIN")"
LOG=$DIR/watch.log
T="$(mktemp -d)"
pass=0; fail=0
RUN="$(date +%s)-$$"

# 测试图片（若无则生成）
if [ ! -f /tmp/shot.png ]; then
  python3 - <<'EOP'
from PIL import Image
Image.new('RGB', (300, 200), (30, 120, 200)).save('/tmp/shot.png')
EOP
fi
if [ ! -f /tmp/big.png ]; then
  python3 - <<'EOP'
import random
from PIL import Image
Image.frombytes('RGB', (1400, 1400), bytes(random.getrandbits(8) for _ in range(1400*1400*3))).save('/tmp/big.png')
EOP
fi
ok(){ echo "  PASS: $1"; pass=$((pass+1)); }
no(){ echo "  FAIL: $1"; fail=$((fail+1)); }
log(){ tr -d '\0' <"$LOG" 2>/dev/null; }
logc(){ log | grep -c -- "$1"; }

cat >"$DIR/server.toml" <<EOT
Listen    = "$SRV"
Psk       = "627ea393638048bc0d5a7554ab58e41e5601e2f4975a214dfc53b500be462a9a"
SignPk    = "c2e46983e667a37d7d8d69679f40f3a05eb8086337693d91dcaf8546d39ddb5e"
EOT
cat >"$DIR/client.toml" <<EOT
Connect   = "$SRV"
Psk       = "627ea393638048bc0d5a7554ab58e41e5601e2f4975a214dfc53b500be462a9a"
SignPk    = "c2e46983e667a37d7d8d69679f40f3a05eb8086337693d91dcaf8546d39ddb5e"
SignSk    = "7599dad4726247d301c00ce0dc0dbfb9144fa958b4e9db30209a8f9d840ac9ca"
EncryptSk = "f313e1fd4ad5fee8841d40ca3d54e14041eb05bf7f4888ad8c800ceb61942db6"
EOT

wait_until(){ local deadline=$(( $(date +%s%N) + $1 * 1000000000 )); shift
  while [ "$(date +%s%N)" -lt "$deadline" ]; do eval "$*" && return 0; sleep 0.1; done; return 1; }

stop_all(){
  # 只杀本测试目录下的二进制，避免误杀 systemd 管理的 ~/.local/bin/piknik
  for p in $(pgrep -x "$(basename "$BIN")"); do
    [ "$(readlink -f "/proc/$p/exe" 2>/dev/null)" = "$BIN" ] && kill "$p" 2>/dev/null
  done
  for p in $(pgrep -x wl-copy); do kill "$p" 2>/dev/null; done
  sleep 0.7
}

# 每条用例的干净起点：服务器跑起来，把剪贴板和服务器的内容都设成同一个基线文本，
# 然后才启动 watcher（这样 watcher 首轮不会把历史残留当成新内容）。
begin_case(){ # $1 = 标签, $2 = 额外 flags
  stop_all
  "$BIN" -server -config "$DIR/server.toml" >"$DIR/server.log" 2>&1 &
  sleep 1
  base="base-$1-$RUN"
  printf '%s' "$base" | wl-copy
  "$BIN" -config "$DIR/client.toml" -copy <<<"$base"   # 服务器基线（带换行）
  sleep 0.5
  : >"$LOG"
  "$BIN" -watch -watch-interval 200ms -config "$DIR/client.toml" $2 >>"$LOG" 2>&1 &
  sleep 1.2
}

server_val(){ "$BIN" -config "$DIR/client.toml" -paste 2>/dev/null; }
clip_text(){ /usr/bin/wl-paste --no-newline 2>/dev/null; }
clip_types(){ /usr/bin/wl-paste --list-types 2>/dev/null | tr '\n' ','; }

echo "=== 1. 文本双向同步（run=$RUN）"
begin_case one ""
U="local-text-$RUN"; wl-copy <<<"$U"
wait_until 6 '[ "$(server_val)" = "'$U'" ]' && ok "本地文本 → 服务器" || no "本地文本推送（server=$(server_val)）"
V="remote-text-$RUN"; "$BIN" -config "$DIR/client.toml" -copy <<<"$V"
wait_until 6 '[ "$(clip_text)" = "'$V'" ]' && ok "服务器文本 → 本地" || no "服务器文本应用（clip=$(clip_text)）"
[ "$(logc 'applied .*(text)')" -ge 1 ] && ok "应用动作有日志" || no "缺少 applied 日志"

echo "=== 2. 自己的推送不被回灌"
begin_case two ""
bad=0
for t in 1 2 3; do
  W="self-$RUN-$t"; wl-copy <<<"$W"
  wait_until 6 '[ "$(server_val)" = "'$W'" ]' || { bad=1; echo "    ($t 未推到服务器)"; }
done
sleep 2   # 留足往返时间让回声有机会发生
[ "$(logc 'applied')" -eq 0 ] && ok "连续三次自推均无回声" \
  || { bad=1; echo "    applied 记录：$(log | grep applied | tr '\n' ' ')"; }
[ "$bad" = "0" ] || no "出现回声回落"

echo "=== 3. 对端回推被截断的 PNG（8 字节，真实事故形态）"
begin_case three ""
S="sentinel-$RUN"; wl-copy <<<"$S"; wait_until 6 '[ "$(server_val)" = "'$S'" ]' || true
before=$(logc ignored)
printf '\x89PNG\r\n\x1a\n' >"$T/bad.png"
"$BIN" -config "$DIR/client.toml" -copy <"$T/bad.png"
wait_until 6 '[ "$(logc ignored)" -gt '$before' ]' && ok "8 字节伪 PNG 被丢弃" || no "未识别出伪 PNG"
sleep 1
[ "$(clip_text)" = "$S" ] && ok "剪贴板未被 8 字节伪 PNG 覆盖" || no "剪贴板被覆盖（$(clip_types)）"

echo "=== 4. 图片同步开启：往返一致且无回声"
begin_case four "-watch-images"
wl-copy --type image/png < /tmp/shot.png
wait_until 8 '[ "$(logc "pushed .*(image)")" -ge 1 ]' || true
sleep 1
/usr/bin/wl-paste --type image/png --no-newline >"$T/rt.png" 2>/dev/null
cmp -s /tmp/shot.png "$T/rt.png" && ok "本地截图往返一致" || no "图片往返不一致（$(wc -c <"$T/rt.png") 字节）"
[ "$(logc applied)" -eq 0 ] && ok "图片推送无回声" || no "图片出现回声（$(logc applied) 次）"
case "$(server_val)" in PiknikImage1\ image/png\ *) ok "服务器存自识别信封";; *) no "服务器内容格式异常";; esac

echo "=== 5. 图片同步关闭：不上传、不落地"
begin_case five ""
wl-copy --type image/png < /tmp/shot.png; sleep 2
[ "$(logc "pushed .*(image)")" -eq 0 ] && ok "关闭时不上传图片" || no "关闭时仍推送图片"
before=$(logc ignored)
"$BIN" -config "$DIR/client.toml" -copy <<<"PiknikImage1 image/png iVBORw0KGgo="
wait_until 6 '[ "$(logc ignored)" -gt '$before' ]' || true
[ "$(clip_types)" = "image/png," ] && ok "关闭时丢弃图片信封，剪贴板仍是原图片" \
  || no "关闭时信封污染了剪贴板（types=$(clip_types)）"

echo "=== 6. 坏信封（base64 与 mime 不符）被丢弃"
begin_case six "-watch-images"
S="env-guard-$RUN"; wl-copy <<<"$S"; wait_until 6 '[ "$(server_val)" = "'$S'" ]' || true
before=$(logc ignored)
printf 'PiknikImage1 image/png %s' "$(head -c 32 /dev/urandom | base64 -w0)" >"$T/bad-env"
"$BIN" -config "$DIR/client.toml" -copy <"$T/bad-env"
wait_until 6 '[ "$(logc ignored)" -gt '$before' ]' && ok "坏信封被丢弃" || no "坏信封未被识别"
sleep 1
[ "$(clip_text)" = "$S" ] && ok "坏信封未覆写剪贴板" || no "剪贴板被坏信封覆写"

echo "=== 7. 大图（5.9 MB PNG，含大量 NUL 字节）"
begin_case seven "-watch-images"
wl-copy --type image/png < /tmp/big.png
wait_until 25 '[ "$(logc "pushed .*(image)")" -ge 1 ]' || true
sleep 1
/usr/bin/wl-paste --type image/png --no-newline >"$T/rt-big.png" 2>/dev/null
cmp -s /tmp/big.png "$T/rt-big.png" && ok "5.9MB PNG 往返一致" || no "大图往返不一致（$(wc -c <"$T/rt-big.png") vs $(wc -c </tmp/big.png)）"

stop_all
rm -rf "$T" "$DIR"
echo
echo "通过 $pass 项，失败 $fail 项"
[ "$fail" = "0" ]
