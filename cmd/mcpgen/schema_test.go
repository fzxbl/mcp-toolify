package main

import "testing"

func TestSnakeCase(t *testing.T) {
	cases := map[string]string{
		"appID":      "app_id",
		"appName":    "app_name",
		"cluster":    "cluster",
		"NewUser":    "new_user",
		"HTTPServer": "http_server",
	}
	for in, want := range cases {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExportName(t *testing.T) {
	cases := map[string]string{
		"app":     "App",
		"appID":   "AppID",
		"cluster": "Cluster",
	}
	for in, want := range cases {
		if got := exportName(in); got != want {
			t.Errorf("exportName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeJSONSchemaDoc(t *testing.T) {
	cases := map[string]string{
		// 正文中的等号必须原样保留（label selector 语法），不再被换成冒号。
		"形如 key=value,key2!=v2 的 labelSelector": "形如 key=value,key2!=v2 的 labelSelector",
		// 反引号 / 双引号仍需替换为单引号，避免破坏 struct tag。
		"用 `fields` 指定": "用 'fields' 指定",
		`带 "引号" 的描述`:    "带 '引号' 的描述",
		// 以 WORD= 开头会被 jsonschema 解析器拒绝，需在首部补空格规避。
		"fields=a,b 指定返回字段": " fields=a,b 指定返回字段",
		"true=可见, false=屏蔽": " true=可见, false=屏蔽",
	}
	for in, want := range cases {
		if got := sanitizeJSONSchemaDoc(in); got != want {
			t.Errorf("sanitizeJSONSchemaDoc(%q) = %q, want %q", in, got, want)
		}
	}
}
