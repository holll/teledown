import argparse
import importlib.util
import os
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from tempfile import TemporaryDirectory
from typing import Dict, Optional
from urllib.parse import urlsplit

from dotenv import dotenv_values
from telethon import TelegramClient

from tools.down_file import down_group
from tools.monit import start_monitor
from tools.sign import batch_sign
from tools.tool import (run_hook, init_db, md5, print_all_channel, print_group, set_chat_alias,
                        parse_message_range, configure_ffmpeg, get_filetype, get_thumb, UserAbort)
from tools.upload_file import get_video_attributes, upload_file

# Telethon 默认为 60 秒：等待时间超过该值的 FLOOD_WAIT 会直接抛错，
# 批量下载时会被当作普通异常跳过该文件。放大到 10 分钟以尽量等满服务端要求的间隔。
FLOOD_SLEEP_THRESHOLD = 600


def parse_range_arg(value: str) -> dict:
    try:
        return parse_message_range(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(str(exc)) from exc


def parse_date_arg(value: str, *, end_of_day: bool = False) -> datetime:
    text = value.strip()
    try:
        if not re.fullmatch(r'[0-9]{4}([-/])[0-9]{2}\1[0-9]{2}(?:[ T][0-9]{2}:[0-9]{2}(?::[0-9]{2})?)?', text):
            raise ValueError
        local = datetime.fromisoformat(text.replace('/', '-'))
        if end_of_day and len(text) == 10:
            local = local.replace(hour=23, minute=59, second=59)
        # Naive input is in the host's local zone; convert using the date's offset.
        return local.astimezone(timezone.utc)
    except (ValueError, OverflowError, OSError) as exc:
        raise argparse.ArgumentTypeError('期望有效的本地日期或时间，例如 2026-09-01 或 "2026-09-01 12:30"') from exc


def build_parser():
    """构建命令行解析器，使用子命令简化分支判断。"""

    parser = argparse.ArgumentParser(description="基于 Telethon 的 Telegram 管理工具")
    parser.add_argument('-c', '--config', default='.env', help='配置文件路径，默认.env')
    parser.add_argument('--proxy', help='代理地址，支持格式：user:pass@ip:port 或 ip:port')

    subparsers = parser.add_subparsers(dest='command', required=True, help='选择要执行的功能')

    subparsers.add_parser('refresh', help='刷新缓存，打印所有频道信息')
    subparsers.add_parser('sign', help='签到')
    subparsers.add_parser('hook', help='执行自定义 Hook 功能')

    download_parser = subparsers.add_parser('download', help='下载文件')
    download_parser.add_argument('-id', required=True, help='频道ID，多个频道用|或,分隔')
    download_parser.add_argument('-user', help='指定下载的用户，支持用逗号或|分隔多个用户，默认下载所有用户',
                                 default=None)
    download_parser.add_argument('--range', type=parse_range_arg, default='>0',
                                 help='消息 ID 范围：>N、<N、N（或 =N）、A..B（含两端）、N,M；默认 >0')
    download_parser.add_argument('--since', type=parse_date_arg, metavar='日期或时间',
                                 help='仅下载此时及之后发送的消息；仅填日期时从本地 00:00:00 起')
    download_parser.add_argument('--until', type=lambda value: parse_date_arg(value, end_of_day=True),
                                 metavar='日期或时间', help='仅下载此时及之前发送的消息；仅填日期时到本地 23:59:59 止')
    download_parser.add_argument('--prefix', help='通配符，文件名前缀', default=None)

    upload_parser = subparsers.add_parser('upload', help='上传文件')
    upload_parser.add_argument('-id', help='频道ID，多个频道用|或,分隔')
    upload_parser.add_argument('-path', required=True, help='上传文件路径')
    upload_parser.add_argument('-dau', default='N', choices=['y', 'Y', 'n', 'N'],
                               help='上传完成后是否删除源文件（Y/N），默认N')
    upload_parser.add_argument('-at', '--addtag', help='上传时增加的标签')

    print_parser = subparsers.add_parser('print', help='打印消息')
    print_parser.add_argument('-id', required=True, help='频道ID，多个频道用|或,分隔')

    monit_parser = subparsers.add_parser('monit', help='监控频道')
    monit_parser.add_argument('-id', required=True, help='频道ID，多个频道用逗号分隔')
    monit_parser.add_argument('-user', help='只监控指定用户的消息，支持用逗号或|分隔多个用户', default=None)
    monit_parser.add_argument('--prefix', help='仅下载匹配通配符的文件名前缀', default=None)

    self_test_parser = subparsers.add_parser('self-test', help='离线检查打包版，无需配置或登录')
    self_test_parser.add_argument('variant', choices=['full', 'noupload'], help='要验证的构建变体')

    return parser


def parse_int(value: Optional[str], field_name: str) -> Optional[int]:
    if value is None or value == '':
        return None
    try:
        return int(value)
    except ValueError:
        print(f"配置项 {field_name} 需要为整数，请检查 .env")
        sys.exit(1)


def parse_alias(alias_raw: Optional[str]) -> Dict[str, str]:
    if not alias_raw:
        return {}

    alias_map: Dict[str, str] = {}
    # 允许用逗号、分号或换行分隔多个 alias，方便在 .env 中编写
    for item in [part.strip() for part in alias_raw.replace('\n', ',').replace(';', ',').split(',')]:
        if not item:
            continue
        if ':' not in item:
            print(f"别名配置 \"{item}\" 无效，需使用 chat_id:别名 的形式")
            sys.exit(1)
        chat_id, title = item.split(':', 1)
        if chat_id and title:
            alias_map[chat_id.strip()] = title.strip()
    return alias_map


def load_config(config_path: str):
    """读取 .env 配置文件并处理常见错误。"""

    if not os.path.exists(config_path):
        print(f"配置文件 {config_path} 不存在，请检查路径")
        sys.exit(1)

    raw_config = dotenv_values(config_path)
    if not raw_config:
        print(f"配置文件 {config_path} 为空或无法解析，请检查内容")
        sys.exit(1)

    return {k.lower(): v for k, v in raw_config.items() if v is not None}


def prepare_proxy(proxy: Optional[str]):
    """解析代理配置，转换为 Telethon 需要的格式。"""

    if not proxy:
        return None

    import python_socks

    username = password = None
    ip_port = proxy
    if '@' in proxy:
        # 解析代理中的认证信息 username:password@ip:port
        user_pass, ip_port = proxy.split('@')
        username, password = user_pass.split(':', 1)
    addr, port = ip_port.rsplit(':', 1)
    return {
        'proxy_type': python_socks.ProxyType.SOCKS5,
        'addr': addr,
        'port': int(port),
        'username': username,
        'password': password,
        'rdns': True  # 使用远程DNS解析，确保可解析Telegram域名
    }


def ensure_login_method(phone: Optional[str], bot_token: Optional[str]):
    """确保只启用一种登录方式。"""

    if (phone and bot_token) or (not phone and not bot_token):
        print('请确认仅配置手机号登录或机器人Token登录其中一种方式')
        sys.exit(1)


async def show_info(client: TelegramClient):
    """登录成功后打印用户信息，确认登录成功。"""

    me = await client.get_me()
    print("-----****************-----")
    print("Name:", me.username)
    print("ID:", me.id)
    print("-----login successful-----")


# ================= 各功能处理函数 =================
async def handle_refresh(client: TelegramClient, args):
    """刷新缓存，打印所有频道信息"""
    await print_all_channel(client)


async def handle_hook(client: TelegramClient, args):
    """执行自定义 Hook 功能"""
    await run_hook(client)


async def handle_sign(client: TelegramClient, args):
    """执行签到功能"""
    await batch_sign(client)


async def handle_download(client: TelegramClient, args):
    """下载频道文件，支持频道id和t.me链接，支持下载范围和用户过滤"""
    target = args.id.strip()
    if target.lower().startswith(('https://', 'http://', 't.me/')):
        try:
            url = urlsplit('https://' + target if target.lower().startswith('t.me/') else target)
            parts = url.path.strip('/').split('/')
            if parts[0] == 'c':
                parts = parts[1:]
            if url.netloc.lower() != 't.me' or not 2 <= len(parts) <= 3 or not all(parts):
                raise ValueError('不是有效的 Telegram 消息链接')
            channels = [parts[0]]
            message_range = parse_message_range('=' + parts[-1])
        except ValueError as exc:
            raise UserAbort(f'无法解析 t.me 消息链接：{exc}') from exc
    else:
        channels = [part.strip() for part in target.replace('|', ',').split(',')]
        if not all(channels):
            raise UserAbort('-id 不能为空，也不能包含空的频道标识')
        message_range = args.range

    failed = False
    for cid in channels:
        try:
            if not await down_group(client, cid, message_range, args.user, args.prefix,
                                    since=args.since, until=args.until):
                failed = True
        except UserAbort:
            raise
        except Exception as exc:
            failed = True
            print(f'频道 {cid} 下载失败：{type(exc).__name__}: {exc}', file=sys.stderr)
    if failed:
        raise UserAbort('本次下载有失败项，请检查各频道的输出')


async def handle_upload(client: TelegramClient, args):
    """上传文件到频道，支持上传路径及上传完成后删除本地文件选项"""
    del_after = args.dau.upper() == 'Y'
    await upload_file(client, args.id, args.path, del_after, args.addtag)


async def handle_print(client: TelegramClient, args):
    """打印频道消息"""
    await print_group(client, args.id)


async def handle_monit(client: TelegramClient, args):
    """监控频道，持续运行直到手动停止"""
    channel_ids = args.id.split(',')
    await start_monitor(client, channel_ids, from_user=args.user, prefix=args.prefix)
    await client.run_until_disconnected()


def check_build_variant(variant: str) -> None:
    excluded = ['pandas', 'tkinter', '_tkinter']
    if variant == 'noupload':
        excluded.extend(('moviepy', 'imageio', 'imageio_ffmpeg', 'PIL', 'magic', 'numpy'))
    if getattr(sys, 'frozen', False):
        bundled = [name for name in excluded if importlib.util.find_spec(name) is not None]
        if bundled:
            raise RuntimeError(f'{variant} 构建意外包含：{", ".join(bundled)}')
    if variant == 'noupload':
        return

    configure_ffmpeg()
    from moviepy import ColorClip

    with TemporaryDirectory(prefix='teledown-self-test-') as temp_dir:
        sample = str(Path(temp_dir) / 'sample.mp4')
        with ColorClip((32, 32), color=(0, 0, 0), duration=0.5) as clip:
            clip.write_videofile(sample, fps=4, codec='libx264', audio=False, logger=None, threads=1)
        mime_type = get_filetype(sample)
        if not mime_type.startswith('video/'):
            raise RuntimeError(f'视频文件 MIME 识别错误：{mime_type}')
        motion = get_video_attributes(sample)
        if abs(motion.duration - 0.5) > 0.01 or (motion.w, motion.h) != (32, 32):
            raise RuntimeError(f'视频属性错误：{motion.duration} 秒，{motion.w}×{motion.h}')
        if not get_thumb(sample).startswith(b'\xff\xd8'):
            raise RuntimeError('视频缩略图不是 JPEG')


def main():
    # ================= 参数解析 =================
    parser = build_parser()
    args = parser.parse_args()
    if args.command == 'download' and args.since and args.until and args.since > args.until:
        parser.error('--since 不能晚于 --until')
    if args.command == 'self-test':
        try:
            check_build_variant(args.variant)
        except Exception as exc:
            print(f'构建自检失败：{type(exc).__name__}: {exc}', file=sys.stderr)
            sys.exit(1)
        print(f'构建自检通过：{args.variant}')
        return

    # ================= 读取配置文件 =================
    config = load_config(args.config)

    api_id = parse_int(config.get('api_id'), 'api_id')
    api_hash = config.get('api_hash')
    phone = config.get('phone')
    bot_token = config.get('bot_token')
    alias = parse_alias(config.get('alias'))

    if not api_id or not api_hash:
        print('请在 .env 中填写 api_id 与 api_hash')
        sys.exit(1)

    # 检查登录方式：手机号登录 或 机器人Token登录，只能二选一
    ensure_login_method(phone, bot_token)

    # 计算登录唯一标识，作为数据库名或会话名
    md5_token = md5(phone or bot_token)

    if alias:
        set_chat_alias(alias)

    # 设置保存文件路径环境变量
    os.environ['save_path'] = config.get('save_path', '')

    # 初始化数据库（根据 md5_token 区分）
    init_db(md5_token)

    # ================= 代理配置 =================
    proxy = prepare_proxy(args.proxy or config.get('proxy'))
    client = TelegramClient(md5_token, api_id, api_hash, proxy=proxy,
                            flood_sleep_threshold=FLOOD_SLEEP_THRESHOLD)

    # ================= 命令映射表 =================
    command_map = {
        'refresh': handle_refresh,
        'hook': handle_hook,
        'sign': handle_sign,
        'download': handle_download,
        'upload': handle_upload,
        'print': handle_print,
        'monit': handle_monit,
    }

    # ================= 主程序入口 =================
    with client.start(phone=phone, bot_token=bot_token):
        # 显示登录信息
        client.loop.run_until_complete(show_info(client))

        # 根据参数执行对应功能，命令互斥，匹配第一个满足条件的执行
        handler = command_map.get(args.command)
        if handler:
            try:
                client.loop.run_until_complete(handler(client, args))
            except UserAbort as e:
                print(e)
                sys.exit(1)
        else:
            print("未匹配任何操作类型，请检查参数")


if __name__ == '__main__':
    main()