为下一个不可变 Artifact 生成一个 JSON 对象形式的文档正文。
只返回文档正文。不要使用 kind、version 或 document_json 等 Artifact 字段包装结果。
替换下面所需 JSON 结构中的占位值，并保留每个键：
{{ .Contract }}

Nasuta 根据这些键确定性渲染 Markdown 章节。不得重命名、合并、省略或新增字段。
所有自然语言字段必须使用简体中文。仅代码标识符、命令、文件路径、API、Schema 名称以及不可翻译的专有名词可保留原文。

目标 Artifact 类型：{{ .Kind }}

输入：
{{ .Input }}
