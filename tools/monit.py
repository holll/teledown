from telethon import TelegramClient, events

from tools.down_file import download_file
from tools.tool import get_chat_title, parse_user_ids


async def start_monitor(client: TelegramClient, channel_ids: list[str], from_user=None, prefix=None):
    channels = []
    channel_title_map = {}
    for channel_id in channel_ids:
        channel = await client.get_entity(int(channel_id))
        channels.append(channel.id)
        channel_title_map[channel.id] = await get_chat_title(client, channel.id)

    target_user_ids = await parse_user_ids(client, from_user)

    @client.on(events.NewMessage(chats=channels))
    async def event_handler(event):
        sender_id = event.message.sender_id
        if target_user_ids and sender_id not in target_user_ids:
            return

        chat_id = event.chat_id
        channel_title = channel_title_map.get(chat_id) or await get_chat_title(client, chat_id)
        message = event.message
        if message.media is not None:
            await download_file(client, channel_title, chat_id, message, prefix=prefix)
        else:
            content = f'From:{channel_title}\n{message.message}'
            await client.send_message(entity='me', message=content)
