import asyncio
import json
import os
from dataclasses import dataclass
from typing import Optional, Literal

from telethon import TelegramClient

ActionType = Literal["send", "click"]


@dataclass
class SignTask:
    bot: str
    action: ActionType
    text: Optional[str] = None  # send/click 前要发送的文本
    button_text: Optional[str] = None  # 要点击的按钮文字
    row: Optional[int] = None  # 也可以按行列点击
    col: Optional[int] = None
    delay: float = 1.5  # 每个任务后的等待时间


def load_sign_tasks_from_file() -> list[SignTask]:
    path = os.getenv("SIGN_TASKS_FILE", "sign_tasks.json")
    if not os.path.exists(path):
        raise RuntimeError(f"签到配置文件不存在: {path}")

    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
    return [
        SignTask(
            bot=item["bot"],
            action=item["action"],
            text=item.get("text"),
            button_text=item.get("button_text"),
            row=item.get("row"),
            col=item.get("col"),
            delay=float(item.get("delay", 1.5)),
        )
        for item in data
    ]


async def send_text(client: TelegramClient, bot: str, text: str):
    await client.send_message(bot, text)


async def click_button_by_text(client: TelegramClient, bot: str, button_text: str, trigger_text: Optional[str] = None):
    """
    如果 trigger_text 提供，则先发送一条消息触发机器人回复，再点击按钮。
    否则直接尝试点击该会话最后一条消息中的按钮。
    """
    if trigger_text:
        await client.send_message(bot, trigger_text)
        await asyncio.sleep(2)
    messages = await client.get_messages(bot, limit=5)
    for msg in messages:
        if not msg.buttons:
            continue
        for r, row_buttons in enumerate(msg.buttons):
            for c, button in enumerate(row_buttons):
                if button_text in getattr(button, "text", None):
                    await msg.click(r, c)
                    return
    raise ValueError(f"未找到按钮: {button_text} in {bot}")


async def click_button_by_index(client: TelegramClient, bot: str, row: int, col: int,
                                trigger_text: Optional[str] = None):
    if trigger_text:
        await client.send_message(bot, trigger_text)
        await asyncio.sleep(2)
    messages = await client.get_messages(bot, limit=5)
    for msg in messages:
        if msg.buttons:
            await msg.click(row, col)
            return
    raise ValueError(f"未找到可点击按钮 in {bot}")


async def run_sign_task(client: TelegramClient, task: SignTask):
    try:
        if task.action == "send":
            if not task.text:
                raise ValueError(f"{task.bot} 的 send 任务缺少 text")
            await send_text(client, task.bot, task.text)
        elif task.action == "click":
            if task.button_text:
                await click_button_by_text(
                    client,
                    task.bot,
                    button_text=task.button_text,
                    trigger_text=task.text,
                )
            elif task.row is not None and task.col is not None:
                await click_button_by_index(
                    client,
                    task.bot,
                    row=task.row,
                    col=task.col,
                    trigger_text=task.text,
                )
            else:
                raise ValueError(f"{task.bot} 的 click 任务缺少 button_text 或 row/col")
        else:
            raise ValueError(f"不支持的 action: {task.action}")
        print(f"[OK] {task.bot}")
    except Exception as e:
        print(f"[FAIL] {task.bot}: {e}")
    await asyncio.sleep(task.delay)


async def batch_sign(client: TelegramClient):
    tasks = load_sign_tasks_from_file()
    for task in tasks:
        await run_sign_task(client, task)
