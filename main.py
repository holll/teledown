import argparse
import os
import sys
from typing import Dict, Optional

from telethon import TelegramClient
from dotenv import dotenv_values
from tools.down_file import down_group
from tools.monit import StartMonit
from tools.tool import Hook, initDb, md5, print_all_channel, print_group
from tools.upload_file import upload_file


def build_parser():
    """构建命令行解析器，使用子命令简化分支判断。"""

    parser = argparse.ArgumentParser(description="基于 Telethon 的 Telegram 管理工具")
    parser.add_argument('-c', '--config', default='.env', help='配置文件路径，默认.env')
    parser.add_argument('--proxy', help='代理地址，支持格式：user:pass@ip:port 或 ip:port')

    subparsers = parser.add_subparsers(dest='command', required=True, help='选择要执行的功能')

    subparsers.add_parser('refresh', help='刷新缓存，打印所有频道信息')
    subparsers.add_parser('hook', help='执行自定义 Hook 功能')

    download_parser = subparsers.add_parser('download', help='下载文件')
    download_parser.add_argument('-id', required=True, help='频道ID，多个频道用|或,分隔')
    download_parser.add_argument('-user', help='指定下载的用户，默认下载所有用户', default=None)
    download_parser.add_argument('--range', default='>0', help='下载范围，默认">0"表示所有消息')
    download_parser.add_argument('--prefix', help='通配符，文件名前缀', default=None)

    upload_parser = subparsers.add_parser('upload', help='上传文件')
    upload_parser.add_argument('-id', help='频道ID，多个频道用|或,分隔')
    upload_parser.add_argument('-path', required=True, help='上传文件路径')
    upload_parser.add_argument('-dau', default='N', choices=['y', 'Y', 'n', 'N'], help='上传完成后是否删除源文件（Y/N），默认N')
    upload_parser.add_argument('-at', '--addtag', help='上传时增加的标签')

    print_parser = subparsers.add_parser('print', help='打印消息')
    print_parser.add_argument('-id', required=True, help='频道ID，多个频道用|或,分隔')

    monit_parser = subparsers.add_parser('monit', help='监控频道')
    monit_parser.add_argument('-id', required=True, help='频道ID，多个频道用逗号分隔')
    monit_parser.add_argument('-user', help='只监控指定用户的消息，支持用逗号或|分隔多个用户', default=None)
    monit_parser.add_argument('--prefix', help='仅下载匹配通配符的文件名前缀', default=None)

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
    for item in alias_raw.split(','):
        item = item.strip()
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
        username, password = user_pass.split(':')
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
    """
    刷新缓存，打印所有频道信息
    """
    print_all_channel(client)

async def handle_hook(client: TelegramClient, args):
    """
    执行自定义 Hook 功能
    """
    await Hook(client)

async def handle_download(client: TelegramClient, args):
    """
    下载频道文件，支持频道id和t.me链接，支持下载范围和用户过滤
    """
    if 't.me' in args.id:
        parts = args.id.split('/')
        channel_id, plus_func = parts[-2], '=' + parts[-1]
    else:
        channel_id, plus_func = args.id, args.range
    for cid in channel_id.split('|'):
        await down_group(client, cid, plus_func, args.user, args.prefix)

async def handle_upload(client: TelegramClient, args):
    """
    上传文件到频道，支持上传路径及上传完成后删除本地文件选项
    """
    del_after = args.dau.upper() == 'Y'
    await upload_file(client, args.id, args.path, del_after, args.addtag)

async def handle_print(client: TelegramClient, args):
    """
    打印频道消息
    """
    await print_group(client, args.id)

async def handle_monit(client: TelegramClient, args):
    """
    监控频道，持续运行直到手动停止
    """
    channel_ids = args.id.split(',')
    await StartMonit(client, channel_ids, from_user=args.user, prefix=args.prefix)
    await client.run_until_disconnected()

def main():
    # ================= 参数解析 =================
    parser = build_parser()
    args = parser.parse_args()

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
    md5Token = md5(phone or bot_token)

    if alias:
        for key, val in alias.items():
            os.environ[key] = val

    # 设置保存文件路径环境变量
    os.environ['save_path'] = config.get('save_path', '')

    # 初始化数据库（根据 md5Token 区分）
    initDb(md5Token)

    # ================= 代理配置 =================
    proxy = prepare_proxy(args.proxy or config.get('proxy'))
    client = TelegramClient(md5Token, api_id, api_hash, proxy=proxy)

    # ================= 命令映射表 =================
    command_map = {
        'refresh': handle_refresh,
        'hook': handle_hook,
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
            client.loop.run_until_complete(handler(client, args))
        else:
            print("未匹配任何操作类型，请检查参数")


if __name__ == '__main__':
    main()
