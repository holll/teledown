from telethon import TelegramClient
from telethon.tl import types,patched

def shorten_filename(filename, limit=50):
    filename = filename.replace('\n', ' ')
    """返回合适长度文件名，中间用...显示"""
    if len(filename) <= limit:
        return filename
    else:
        return filename[:int(limit / 2) - 3] + '...' + filename[len(filename) - int(limit / 2):]


def print_all_channel(client: TelegramClient, need_type: types):
    for d in client.iter_dialogs():
        channelId = d.entity.id
        channelName = d.name
        if isinstance(d.entity, need_type):
            print(d.entity.title, d.entity.id)


async def getHistoryMessage(client: TelegramClient, chat_id: int):
    channelData = await client.get_entity(chat_id)
    channel_title = channelData.title
    messages = client.iter_messages(chat_id)
    return channel_title, messages


def can_continue(_id, plus_func):
    need_id = 0
    if plus_func[0] != '|':
        need_id = int(plus_func[1:])
    if plus_func[0] == '=':
        if need_id > _id:
            return 2
        elif need_id < _id:
            return 1
        else:
            return 0
    elif plus_func[0] == '>':
        if need_id < _id:
            return 0
        else:
            return 2
    elif plus_func[0] == '<':
        if need_id > _id:
            return 0
        else:
            return 1
    elif plus_func[0] == '|':
        tmpId = plus_func[1:].split('|')
        a_id = int(tmpId[0])
        b_id = int(tmpId[-1])
        if a_id >= b_id:
            print('消息id范围错误')
            return 2
        if _id > tmpId[-1]:
            return 1
        elif _id < tmpId[0]:
            return 2
        else:
            return 0
    else:
        return 2