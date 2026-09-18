# 自定义字段功能使用指南

## 概述

自定义字段功能允许用户在清单文件（manifest）中添加标准结构之外的自定义XML属性，这些属性会被解析并存储在相应结构体的`CustomAttrs`字段中。

## 支持自定义字段的结构体

以下结构体支持自定义字段：

- `Manifest`：清单文件根元素
- `Remote`：远程Git服务器
- `Default`：默认设置
- `Project`：Git项目
- `Include`：包含的清单文件
- `RemoveProject`：要移除的项目
- `Copyfile`：要复制的文件
- `Linkfile`：要链接的文件

## 使用方法

### 在XML中添加自定义属性

在XML清单文件中，您可以直接在任何支持的元素上添加自定义属性：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<manifest custom-version="1.0">
  <remote name="origin" fetch="https://example.com/" priority="high" />
  <default remote="origin" revision="master" build-type="release" />
  <project name="project1" path="path/to/project1" owner="team-a">
    <copyfile src="src/file" dest="dest/file" permission="0644" />
    <linkfile src="src/link" dest="dest/link" description="配置文件链接" />
  </project>
  <include name="other.xml" optional="true" />
  <remove-project name="removed-project" reason="deprecated" />
</manifest>
```

### 在代码中访问自定义属性

所有支持自定义字段的结构体都有一个`CustomAttrs`字段（类型为`map[string]string`）和一个`GetCustomAttr`方法，可以用来访问自定义属性：

```go
// 解析清单文件
parser := manifest.NewParser()
manifest, err := parser.ParseFromFile("manifest.xml", nil)
if err != nil {
    // 处理错误
}

// 访问Manifest的自定义属性
if version, ok := manifest.GetCustomAttr("custom-version"); ok {
    fmt.Printf("清单版本: %s\n", version)
}

// 访问Remote的自定义属性
for _, remote := range manifest.Remotes {
    if priority, ok := remote.GetCustomAttr("priority"); ok {
        fmt.Printf("远程仓库 %s 的优先级: %s\n", remote.Name, priority)
    }
}

// 访问Project的自定义属性
for _, project := range manifest.Projects {
    if owner, ok := project.GetCustomAttr("owner"); ok {
        fmt.Printf("项目 %s 的所有者: %s\n", project.Name, owner)
    }
}
```

## 注意事项

1. 自定义属性的名称不应与标准属性冲突，否则会被忽略。
2. 自定义属性的值始终以字符串形式存储，需要时应进行类型转换。
3. 自定义属性在 XML 序列化（`ToXML`）时会被保留：根级 `Manifest` 的 `CustomAttrs` 会按键名排序输出（internal/manifest/manifest.go:2087-2095 的根级属性序列化逻辑）。子结构体 `Remote`、`Project`、`Default`、`RemoveProject`、`Copyfile`、`Linkfile` 的自定义属性同样会按键名排序输出（`ToXML` 中各元素序列化段，manifest.go:2138、2160、2203、2221、2231、2253 附近的 `sortedKeys` 循环）。其中 `Project` 会跳过以 `__` 开头的内部属性（如 `__remote_fetch`、`__remote_url`，manifest.go:2203-2208）；`Include` 元素在 `ToXML` 中不输出（init 合并后不再需要 include 标签，manifest.go:2166），故其自定义属性不参与序列化。

## 示例应用场景

- 为项目添加元数据：如所有者、优先级、分类等
- 添加构建相关信息：如构建类型、目标平台等
- 添加权限或安全相关属性：如访问级别、所需权限等
- 添加依赖关系信息：如依赖版本、兼容性要求等