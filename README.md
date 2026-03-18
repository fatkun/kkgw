# kkapi_test

一个用于测试多个 API URL 延迟并可选完成 AI 对话连通性验证、自动写入 codex 配置的小工具。

# 使用说明

先在[Release](https://github.com/fatkun/kkgw/releases)下载，根据系统架构下载文件

| 系统    | 架构               | 文件名                   |
| ------- | ------------------ | ------------------------ |
| Windows | 64位               | kkapi_windows_amd64.zip  |
| Linux   | 64位               | kkapi_linux_amd64.tar.gz |
| Mac     | amd64位(Inter芯片) | kkapi_macos_amd64.tar.gz |
| Mac     | arm64位            | kkapi_macos_arm64.tar.gz |


下载后，解压执行可执行文件（例如：kkapi_windows_amd64.exe）

如果是Windows，会提示 Windows 已保护你的电脑。
点击`更多信息`，选择`仍要运行`。

启动后，会开始测试延迟，测试延迟后会提醒你“推荐使用地址”。

然后会询问`是否测试AI对话`，按回车默认测试，按N取消测试。
```
推荐使用地址:
https://xxxxxx/v1
是否测试AI对话（Y/n）
```
如果按了回车，会要求你填入密钥。如果是下面的提示，表示请求正常。
```
AI 检测成功，返回内容：
Hi—what can I help you with?
```

然后提示你`是否配置codex`，会以当前的API地址写入codex的配置文件。默认回车会配置，输入N表示不配置。
```
是否配置codex(Y/n)
```
如果前面没测试AI对话，这里会要求你输入密钥。然后提示你输入使用的模型，默认是gpt-5.2-codex，不用填写，直接按回车就行。
```
使用模型（默认：gpt-5.2-codex）
```

显示这样就配置完成了
```
codex 配置完成。
配置路径：C:\Users\xxxx\.codex\config.toml
```

如果之前打开了codex或者vscode，需要关闭后重新打开才会使用新的配置文件。
