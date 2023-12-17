import argparse
import json
import os
import sys

from telethon import TelegramClient

from tools.down_file import down_group
from tools.monit import StartMonit
from tools.tool import print_all_channel, Hook, print_group, initDb, md5
from tools.upload_file import upload_file

# 1.创建解释器
parser = argparse.ArgumentParser()
# 2.创建一个互斥参数组
mutex_group = parser.add_mutually_exclusive_group()
# 3.添加需要的参数
parser.add_argument('-c', '--config', default='config.json', help='配置文件')
parser.add_argument('-re', '--refresh', action='store_true', help='刷新缓存')
mutex_group.add_argument('-up', '--upload', action='store_true', help='上传文件')
mutex_group.add_argument('-down', '--download', action='store_true', help='下载文件')
mutex_group.add_argument('-print', action='store_true', help='打印消息')
mutex_group.add_argument('-m', '--monit', action='store_true', help='监控频道')
parser.add_argument('-id', help='频道ID')
parser.add_argument('-user', help='指定下载用户', default=None)
parser.add_argument('--range', default='>0', help='下载范围')
parser.add_argument('-path', help='上传路径')
parser.add_argument('-dau', default='N', choices=['y', 'Y', 'n', 'N'], help='上传完成删除原文件')
parser.add_argument('-at', '--addtag', help='增加tag')
# 3.进行参数解析
args = parser.parse_args()
config_path = args.config
# 配置处理开始
with open(config_path, 'r', encoding='utf-8') as f:
    config = json.load(f)
api_id = config.get('api_id')
api_hash = config.get('api_hash')
phone = config.get('phone')
bot_token = config.get('bot_token')
alias = config.get('alias')
if (phone is not None and bot_token is not None) or (phone is None and bot_token is None):
    print('请确认使用机器人登录还是电话号码登录')
    exit()
if phone:
    md5Token = md5(phone)
else:
    md5Token = md5(bot_token)
if alias:
    for _id in alias:
        os.environ[_id] = alias[_id]
initDb(md5Token)
os.environ['save_path'] = save_path = config.get('save_path')
proxy_ip = config.get('proxy_ip')
proxy_port = config.get('proxy_port')
if proxy_port is not None:
    import socks

    if proxy_ip is None:
        proxy_ip = '127.0.0.1'
    client = TelegramClient(md5Token, api_id, api_hash, proxy=(socks.SOCKS5, proxy_ip, proxy_port))
else:
    client = TelegramClient(md5Token, api_id, api_hash)


# 配置处理结束


async def upDate_dialogs():
    await client.get_dialogs()


# 展示登陆的信息
def show_my_inf(me):
    print("-----****************-----")
    print("Name:", me.username)
    print("ID:", me.id)
    print("-----login successful-----")


async def client_main():
    print("-client-main-")
    me = await client.get_me()
    show_my_inf(me)


if __name__ == '__main__':
    # 除了刷新缓存，都需要频道ID
    if not args.refresh and args.id is None:
        sys.exit(1)
    if args.upload and args.path is None:
        print('缺失上传路径')
        sys.exit(1)
    with client.start(phone=phone, bot_token=bot_token):
        client.loop.run_until_complete(client_main())
        client.loop.run_until_complete(Hook(client))
        if args.refresh:
            print_all_channel(client=client)
        if args.download:
            if 't.me' in args.id:
                tmpList = args.id.split('/')
                channel_id = tmpList[-2]
                plus_func = tmpList[-1]
            else:
                channel_id = args.id
                plus_func = args.range
            for _id in channel_id.split('|'):
                client.loop.run_until_complete(down_group(client, _id, plus_func, args.user))
        elif args.upload:
            del_after_upload = True if args.dau.upper() == 'Y' else False
            client.loop.run_until_complete(upload_file(client, args.id, args.path, del_after_upload, args.addtag))
        elif args.print:
            client.loop.run_until_complete(print_group(client, args.id))
        elif args.monit:
            channel_ids = args.id.split(',')
            client.loop.run_until_complete(StartMonit(client, channel_ids))
            client.run_until_disconnected()
