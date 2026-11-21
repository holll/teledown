from telethon import TelegramClient, events, utils

from tools.down_file import download_file
from tools.tool import get_chat_title, matches_sender, parse_user_ids, UserAbort


async def start_monitor(client: TelegramClient, channel_ids: list[str], from_user=None, prefix=None):
    channels = []
    channel_title_map = {}
    for channel_id in channel_ids:
        try:
            cid = int(channel_id) if channel_id.lstrip('-').isdigit() else channel_id
            channel = await client.get_entity(cid)
        except Exception as e:
            print(f"无法获取频道 {channel_id}: {e}")
            continue
        cid = utils.get_peer_id(channel)
        channels.append(cid)
        channel_title_map[cid] = await get_chat_title(client, cid)

    if not channels:
        raise UserAbort('没有可监控的频道')
    target_user_ids = await parse_user_ids(client, from_user)

    @client.on(events.NewMessage(chats=channels))
    async def event_handler(event):
        if not matches_sender(event.message.sender_id, target_user_ids):
            return

        chat_id = event.chat_id
        channel_title = channel_title_map.get(chat_id) or await get_chat_title(client, chat_id)
        message = event.message
        if message.media is not None:
            await download_file(client, channel_title, chat_id, message, prefix=prefix)
        elif message.message:
            content = f'From:{channel_title}\n{message.message}'
            await client.send_message(entity='me', message=content)
