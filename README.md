# teledown

使用Telethon批量下载telegram中任意群组/频道文件

## 使用教程

1. 克隆或下载本项目代码。
2. 将 `config.example.json` 重命名为 `config.json` 并按下文说明填写。
3. 创建并激活 Python 3 虚拟环境（可选，但推荐）。
4. 安装依赖 [教程](#jump2)。
5. 阅读命令行参数的作用 [教程](#jump3)，并按示例运行。

## 配置文件解释<a id="jump1"></a>

```json
{
  "api_id": "参见下方教程",
  "api_hash": "参见下方教程",
  "phone": "手机号（与 bot_token 任填一项）",
  "bot_token": "机器人登录 Token（与 phone 二选一）",
  "save_path": "下载文件的保存路径",
  "proxy": "可选，形如 user:pass@ip:port 或 ip:port",
  "alias": { "-100123456": "给指定频道设置显示名" }
}
```

### 获取 api_id 和 api_hash

可参考 https://www.jianshu.com/p/3d047c7516cf ，在 https://my.telegram.org 申请后填写到配置中。

## 依赖安装教程<a id="jump2"></a>

基础运行环境：Python 3.8+。

1. 安装基础依赖（必需）

```bash
pip3 install -r requirements.txt
```

2. 如果需要开启代理参数 `--proxy`，请确保安装了 `python-socks`（已包含在 `requirements.txt`）。

3. 上传功能需要的附加依赖（可选）：

```bash
pip3 install -r requirements_upload.txt
# Linux 安装 python-magic
pip3 install python-magic
# Windows 安装 python-magic-bin
pip3 install python-magic-bin==0.4.14
```

4. 如果希望更快的下载速度，建议额外安装 `cryptg`（已列在 `requirements.txt`，若未自动安装请手动执行 `pip3 install cryptg`）。

## 可选参数<a id="jump3"></a>

CLI 已改为子命令模式，先选择操作类型，再填写对应参数：

```
python main.py [-c config.json] [--proxy user:pass@ip:port]
               {refresh,hook,download,upload,print,monit} ...

子命令 refresh：刷新缓存，打印所有频道信息
子命令 hook：执行自定义 Hook 功能
子命令 download：
  -id     频道ID，多个频道用|或,分隔；支持 https://t.me/xxx/123 链接
  -user   指定下载的用户，默认下载所有用户
  --range 下载范围，默认">0"表示所有消息
  --prefix 通配符，文件名前缀
子命令 upload：
  -id     频道ID，多个频道用|或,分隔
  -path   上传文件路径
  -dau    上传完成后是否删除源文件（Y/N），默认N
  -at     上传时增加的标签
子命令 print：
  -id     频道ID，多个频道用|或,分隔
子命令 monit：
  -id     频道ID，多个频道用逗号分隔
  -user   仅监控指定用户的消息，支持用逗号或|分隔多个用户，默认所有用户
  --prefix  监控时仅下载匹配通配符的文件名前缀
```

快速开始：

```bash
# 复制配置模版并填写 api_id / api_hash 以及 phone 或 bot_token（二选一）
cp config.example.json config.json

# 安装依赖（建议在虚拟环境内执行）
pip3 install -r requirements.txt

# 首次运行会引导输入验证码或二步验证密码
python main.py refresh
```

### 示例命令

刷新缓存并打印所有频道信息：

```bash
python main.py refresh
```

下载两个频道的全部文件，并只保留文件名前缀为 `report` 的文件：

```bash
python main.py download -id 123456789|https://t.me/example_channel/1 --prefix "report*"
```

仅下载指定用户的消息范围，示例中下载 `@someone` 在频道 1111 里的消息 10~200：

```bash
python main.py download -id 1111 -user @someone --range 10..200
```

上传本地目录的文件到多个频道并在完成后删除源文件：

```bash
python main.py upload -id 1111,2222 -path /data/files -dau Y -at "#归档"
```

实时监控频道并在新文件出现时触发下载，并只关注指定用户的文件（多个用户用逗号分隔）：

```bash
python main.py monit -id 3333,4444 -user @someone,@another --prefix "*.pdf"
```

## 注意事项

1. 如果你想下载速度更快，`pip3 install cryptg `
2. 如果使用上传功能，需要安装额外的依赖。Linux系统安装`python-magic`、Windows系统安装`python-magic-bin==0.4.14`
3. 本程序仅支持python3

## 常见错误

1. `AttributeError: module 'socks' has no attribute 'SOCKS5'`，请确认安装了PySocks
2. `Server sent a very new message with ID xxxxxxxxxxxxxxxxxxx, ignoring`
   ，如果你确定没有在多个设备上使用同一个session文件，那么这个问题是由于当前设备时间与telegram服务器时间差距过大（大于30s）。
   解决办法：服务器时间戳是`xxxxxxxxxxxxxxxxxxx >> 32`，请将本地时间戳修改到与服务器时间戳相差30s以内
3. `Could not find the input entity for XXXXXXX`，请先刷新缓存

## Star历史

<a href="https://github.com/holll/teledown/stargazers">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=holll/teledown&type=Date&theme=dark" />
    <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=holll/teledown&type=Date" />
    <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=holll/teledown&type=Date" />
  </picture>
</a>
