import argparse
import json
import os
import sys

from telethon import TelegramClient
from tools.down_file import down_group
from tools.monit import StartMonit
from tools.tool import print_all_channel, Hook, print_group, initDb, md5
from tools.upload_file import upload_file

# ================= 参数解析 =================
parser = argparse.ArgumentParser(description="基于 Telethon 的 Telegram 管理工具")
mutex_group = parser.add_mutually_exclusive_group()

# 通用参数
parser.add_argument('-c', '--config', default='config.json', help='配置文件路径，默认config.json')
parser.add_argument('-re', '--refresh', action='store_true', help='刷新缓存，打印所有频道信息')
parser.add_argument('-hook', '--hook', action='store_true', help='执行自定义 Hook 功能')

# 互斥操作参数
mutex_group.add_argument('-up', '--upload', action='store_true', help='上传文件')
mutex_group.add_argument('-down', '--download', action='store_true', help='下载文件')
mutex_group.add_argument('-print', action='store_true', help='打印消息')
mutex_group.add_argument('-m', '--monit', action='store_true', help='监控频道')

# 其他参数
parser.add_argument('-id', help='频道ID，多个频道用|或,分隔')
parser.add_argument('-user', help='指定下载的用户，默认下载所有用户', default=None)
parser.add_argument('--range', default='>0', help='下载范围，默认">0"表示所有消息')
parser.add_argument('--prefix', help='通配符，文件名前缀', default=None)
parser.add_argument('-path', help='上传文件路径')
parser.add_argument('-dau', default='N', choices=['y', 'Y', 'n', 'N'], help='上传完成后是否删除源文件（Y/N），默认N')
parser.add_argument('-at', '--addtag', help='上传时增加的标签')
parser.add_argument('--proxy', help='代理地址，支持格式：user:pass@ip:port 或 ip:port')

# 解析命令行参数
args = parser.parse_args()

# ================= 读取配置文件 =================
try:
    with open(args.config, 'r', encoding='utf-8') as f:
        config = json.load(f)
except FileNotFoundError:
    print(f"配置文件 {args.config} 不存在，请检查路径")
    sys.exit(1)
except json.JSONDecodeError:
    print(f"配置文件 {args.config} 格式错误，请检查JSON语法")
    sys.exit(1)

api_id = config.get('api_id')
api_hash = config.get('api_hash')
phone = config.get('phone')
bot_token = config.get('bot_token')
alias = config.get('alias')

# 检查登录方式：手机号登录 或 机器人Token登录，只能二选一
if (phone and bot_token) or (not phone and not bot_token):
    print('请确认仅配置手机号登录或机器人Token登录其中一种方式')
    sys.exit(1)

# 计算登录唯一标识，作为数据库名或会话名
md5Token = md5(phone or bot_token)

if alias:
    for key, val in alias.items():
        if key and val:
            os.environ[key] = val

# 设置保存文件路径环境变量
os.environ['save_path'] = config.get('save_path', '')

# 初始化数据库（根据 md5Token 区分）
initDb(md5Token)

# ================= 代理配置 =================
proxy = args.proxy or config.get('proxy')
if proxy:
    import python_socks

    username = password = None
    ip_port = proxy
    if '@' in proxy:
        # 解析代理中的认证信息 username:password@ip:port
        user_pass, ip_port = proxy.split('@')
        username, password = user_pass.split(':')
    addr, port = ip_port.rsplit(':', 1)
    proxy = {
        'proxy_type': python_socks.ProxyType.SOCKS5,
        'addr': addr,
        'port': int(port),
        'username': username,
        'password': password,
        'rdns': True  # 使用远程DNS解析，确保可解析Telegram域名
    }
    client = TelegramClient(md5Token, api_id, api_hash, proxy=proxy)
else:
    client = TelegramClient(md5Token, api_id, api_hash)

# ================= 登录成功信息展示 =================
async def show_info():
    """
    登录成功后打印用户信息，确认登录成功
    """
    me = await client.get_me()
    print("-----****************-----")
    print("Name:", me.username)
    print("ID:", me.id)
    print("-----login successful-----")

# ================= 各功能处理函数 =================
async def handle_refresh(args):
    """
    刷新缓存，打印所有频道信息
    """
    print_all_channel(client)

async def handle_hook(args):
    """
    执行自定义 Hook 功能
    """
    await Hook(client)

async def handle_download(args):
    """
    下载频道文件，支持频道id和t.me链接，支持下载范围和用户过滤
    """
    if not args.id:
        print("下载模式下需要提供 -id 参数")
        return
    if 't.me' in args.id:
        parts = args.id.split('/')
        channel_id, plus_func = parts[-2], '=' + parts[-1]
    else:
        channel_id, plus_func = args.id, args.range
    for cid in channel_id.split('|'):
        await down_group(client, cid, plus_func, args.user, args.prefix)

async def handle_upload(args):
    """
    上传文件到频道，支持上传路径及上传完成后删除本地文件选项
    """
    if not args.path:
        print("上传模式下需要提供 -path 参数")
        return
    del_after = args.dau.upper() == 'Y'
    await upload_file(client, args.id, args.path, del_after, args.addtag)

async def handle_print(args):
    """
    打印频道消息
    """
    if not args.id:
        print("打印消息模式下需要提供 -id 参数")
        return
    await print_group(client, args.id)

async def handle_monit(args):
    """
    监控频道，持续运行直到手动停止
    """
    if not args.id:
        print("监控模式下需要提供 -id 参数")
        return
    channel_ids = args.id.split(',')
    await StartMonit(client, channel_ids)
    await client.run_until_disconnected()

# ================= 命令映射表 =================
command_map = [
    (lambda a: a.refresh, handle_refresh),
    (lambda a: a.hook, handle_hook),
    (lambda a: a.download, handle_download),
    (lambda a: a.upload, handle_upload),
    (lambda a: a.print, handle_print),
    (lambda a: a.monit, handle_monit),
]

# ================= 主程序入口 =================
if __name__ == '__main__':
    # 参数校验，保证必传参数至少一项
    if not any([args.refresh, args.hook, args.id]):
        print("参数不足，必须至少包含 -re / -hook / -id 之一")
        sys.exit(1)

    # 启动客户端，完成登录
    with client.start(phone=phone, bot_token=bot_token):
        # 显示登录信息
        client.loop.run_until_complete(show_info())

        # 根据参数执行对应功能，命令互斥，匹配第一个满足条件的执行
        for condition, handler in command_map:
            if condition(args):
                client.loop.run_until_complete(handler(args))
                break
        else:
            print("未匹配任何操作类型，请检查参数")
