import json
import os
import sys

import socks
from telethon import TelegramClient

from tools.down_file import down_group
from tools.monit import StartMonit
from tools.tool import print_all_channel, Hook, print_group, initDb, md5
from tools.upload_file import upload_file

config_path = './config.json'
# 配置处理开始
# These example values won't work. You must get your own api_id and
# api_hash from https://my.telegram.org, under API Development.
with open(config_path, 'r', encoding='utf-8') as f:
    config = json.load(f)
api_id = config.get('api_id')
api_hash = config.get('api_hash')
phone = config.get('phone')
bot_token = config.get('bot_token')
if (phone is not None and bot_token is not None) or (phone is None and bot_token is None):
    print('请确认使用机器人登录还是电话号码登录')
    exit()
if phone:
    md5Token = md5(phone)
else:
    md5Token = md5(bot_token)
initDb(md5Token)
os.environ['save_path'] = save_path = config.get('save_path')
proxy_ip = config.get('proxy_ip')
proxy_port = config.get('proxy_port')
if proxy_port is not None:
    if proxy_ip is None:
        proxy_ip = '127.0.0.1'
    client = TelegramClient(md5Token, api_id, api_hash, proxy=(socks.SOCKS5, proxy_ip, proxy_port))
else:
    client = TelegramClient(md5Token, api_id, api_hash)
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
    with client.start(phone=phone, bot_token=bot_token):
        client.loop.run_until_complete(client_main())
        client.loop.run_until_complete(Hook(client))
        plus_func = '>0'
        if len(sys.argv) == 1:
            select = input('功能选择：\n1、查看所有频道\n2、下载频道资源\n3、上传频道资源\n4、查看频道资源\n5、监控模式\n')
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
            print_all_channel(client=client)
        elif select == '2':
            if channel_id is None:
                channel_id = input('频道id：')
            client.loop.run_until_complete(down_group(client, channel_id, plus_func))
        elif select == '3':
            channel_id = input('上传到：')
            folder_path = input('文件（夹）路径：')
            while True:
                del_after_upload = input('上传后删除原文件(Y/N)：').upper()
                if del_after_upload == 'Y':
                    del_after_upload = True
                    break
                elif del_after_upload == 'N':
                    del_after_upload = False
                    break
                else:
                    print("无效的输入，请重新输入")
            client.loop.run_until_complete(upload_file(client, channel_id, folder_path,del_after_upload))
        elif select == '4':
            chat_id = input('请输入频道id:')
            client.loop.run_until_complete(print_group(client, chat_id))
        elif select == '5':
            channel_ids = input('请输入需要监控的频道(以,分割):\n').split(',')
            client.loop.run_until_complete(StartMonit(client, channel_ids))
            client.run_until_disconnected()
