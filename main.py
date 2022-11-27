import datetime
import json
import os

import socks
import telethon.tl.custom
from telethon import TelegramClient

import tools.execSql as execSql
from tools.tool import getHistoryMessage

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


def analInfo(message: telethon.tl.custom.Message):
    title = ''
    viewKey = ''
    author = ''
    date = '1970-01-01'
    needPay = 0
    messageId = message.id
    url = 'https://t.me/c/%s/' % message.peer_id.channel_id
    if message.message is None:
        return title, viewKey, author, date, messageId, needPay, False
    rawInfo = message.message.split('\n')
    rawInfoLen = len(rawInfo)
    if rawInfoLen < 3 or rawInfoLen > 5 or 'Viewkey' not in message.message:
        print('信息格式异常\n', rawInfo, url + str(message.id))
        return title, viewKey, author, date, messageId, needPay, False
    if rawInfoLen == 5:
        if '付费' in rawInfo[0]:
            needPay = 1
            rawInfo.pop(0)
            rawInfoLen = len(rawInfo)
        else:
            print('信息格式异常', rawInfo, url + str(message.id))
            return title, viewKey, author, date, messageId, needPay, False
    if rawInfoLen == 3:
        rawInfo.insert(0, '')
        # rawInfoLen = len(rawInfo)
    title = rawInfo[0].strip()
    viewKey = rawInfo[1].split(':')[-1].strip()
    author = rawInfo[2].split('#')[-1].strip()
    date = datetime.datetime.strptime(rawInfo[3].split('on')[-1].strip(), "%Y%m%d").strftime("%Y-%m-%d")
    return title, viewKey, author, date, messageId, needPay, True


async def collect_group(client: TelegramClient, chat_id: str, isAll: bool):
    db = execSql.ReadSQL('./data/data.db')
    maxId = db.getMaxId()
    if maxId is None:
        maxId = 0
    channel_title, messages = await getHistoryMessage(client, int(chat_id))
    async for message in messages:
        if message.id <= maxId and isAll:
            break
        title, viewKey, author, date, _id, needPay, status = analInfo(message)
        if status:
            print(title, viewKey, author, date)
            if not db.insertDb(title, viewKey, author, date, _id, needPay):
                print(message.message)
        else:
            break
    print(channel_title, '全部下载完成')
    db.conn.commit()
    db.closeDb()


if __name__ == '__main__':
    with client:
        client.loop.run_until_complete(client_main())
        client.loop.run_until_complete(collect_group(client, '1178953801', True))
