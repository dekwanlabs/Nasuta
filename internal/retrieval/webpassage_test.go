package retrieval

import (
	"strings"
	"testing"
)

func TestSelectWebPassagesPrefersQuestionRelevantSections(t *testing.T) {
	page := `# 黄金市场年度报告

## 网站导航

首页 市场 行情 登录 注册 联系我们 网站地图，访问其他频道获取更多服务信息。

## 央行购金

全球央行持续增加黄金储备，央行购金需求成为支撑黄金价格的重要因素。地缘政治风险也提高了避险需求。

## 珠宝消费

珠宝门店推出新品并调整陈列方式，消费者可以在线预约到店试戴和咨询售后服务。

## 美元与利率

实际利率下降和美元走弱通常有利于黄金价格，市场对降息路径的预期会影响后续走势。`

	got := SelectWebPassages(page, "央行购金为什么支撑黄金价格", 500)
	if len(got.Passages) == 0 {
		t.Fatal("SelectWebPassages() returned no passages")
	}
	if got.Passages[0].Heading != "央行购金" {
		t.Fatalf("first heading = %q, want 央行购金", got.Passages[0].Heading)
	}
	if strings.Contains(got.Passages[0].Content, "登录 注册") {
		t.Fatalf("navigation content ranked first: %q", got.Passages[0].Content)
	}
}

func TestSelectWebPassagesDeduplicatesAndCapsSections(t *testing.T) {
	page := `# 故障说明

## 数据库连接

数据库连接超时通常与连接池耗尽有关，需要检查活跃连接和等待队列。

数据库连接超时通常与连接池耗尽有关，需要检查活跃连接和等待队列。

连接池配置还应结合请求并发量检查，不能只增加最大连接数而忽略慢查询。

第三段继续讨论数据库连接池耗尽和等待队列增长，并给出监控连接使用率的方法。`

	got := SelectWebPassages(page, "数据库连接池耗尽", 1000)
	if got.TotalPassages != 3 {
		t.Fatalf("total passages = %d, want 3 unique passages", got.TotalPassages)
	}
	if len(got.Passages) > 2 {
		t.Fatalf("selected %d passages from one section, want at most 2", len(got.Passages))
	}
}

func TestSelectWebPassagesRejectsLinkHeavyNavigation(t *testing.T) {
	page := `# 产品文档

首页 (https://example.com/) 产品 (https://example.com/products) 登录 (https://example.com/login) 注册 (https://example.com/signup) 联系我们 (https://example.com/contact)

## 故障处理

连接超时需要先确认服务端状态，再检查客户端连接池和网络请求耗时。`

	got := SelectWebPassages(page, "连接超时怎么处理", 500)
	if got.TotalPassages != 1 {
		t.Fatalf("total passages = %d, want only the content passage", got.TotalPassages)
	}
	if len(got.Passages) != 1 || got.Passages[0].Heading != "故障处理" {
		t.Fatalf("selected passages = %#v", got.Passages)
	}
}
