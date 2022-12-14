import json
import os
import sys

import socks
from telethon import TelegramClient
from telethon.tl import types

from tools.down_file import down_group
from tools.tool import print_all_channel

config_path = './config.json'
# 配置处理开始
# These example values won't work. You must get your own api_id and
# api_hash from https://my.telegram.org, under API Development.
with open(config_path, 'r', encoding='utf-8') as f:
    config = json.load(f)
api_id = config.get('api_id')
api_hash = config.get('api_hash')
os.environ['save_path'] = save_path = config.get('save_path')
proxy_port = config.get('proxy_port')
if proxy_port is not None:
    client = TelegramClient('python', api_id, api_hash, proxy=(socks.SOCKS5, 'localhost', proxy_port))
else:
    client = TelegramClient('python', api_id, api_hash)
# 配置处理结束


# 接受监视的媒体格式(tg里面直接发送gif最后是mp4格式！)，如果需要下载mp4内容可以添加"image/mp4"
accept_file_format = ["image/jpeg", "image/gif", "image/png", "image/webp", "video/mp4"]


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
    with client:
        client.loop.run_until_complete(client_main())
        plus_func = '>0'
        if len(sys.argv) == 1:
            select = input('功能选择：\n1、查看所有频道\n2、下载频道资源\n')
            channel_id = None
        else:
            select = '2'
            if 't.me' in sys.argv[1]:
                tmpList = sys.argv[1].split('/')
                channel_id = tmpList[-2]
                plus_func = '=' + tmpList[-1]
            else:
                channel_id = sys.argv[1]
                if len(sys.argv) == 3:
                    plus_func = sys.argv[2]
        if select == '1':
            print_all_channel(client=client, need_type=types.Channel)
        else:
            if channel_id is None:
                channel_id = input('频道id：')
            client.loop.run_until_complete(down_group(client, channel_id, plus_func))
