package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	_ "github.com/glebarez/go-sqlite" // 使用纯Go实现的SQLite驱动，支持交叉编译
)

const maxKeywordResults = 2000

func main() {
	dbPath, err := resolveDBPath()
	if err != nil {
		log.Fatalf("resolve database path: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("ping database: %v", err)
	}

	s := server.NewMCPServer(
		"IPSearch",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	ipLookupTool := mcp.NewTool(
		"ip_lookup",
		mcp.WithDescription("根据IPv4地址查询所属IP段以及IPWhois信息"),
		mcp.WithString(
			"ip",
			mcp.Required(),
			mcp.Description("待查询的IPv4地址"),
		),
	)
	s.AddTool(ipLookupTool, ipLookupHandler(db))

	keywordLookupTool := mcp.NewTool(
		"keyword_lookup",
		mcp.WithDescription("根据IPWhois登记信息关键字搜索IP段以及IPWhois信息"),
		mcp.WithString(
			"keywords",
			mcp.Required(),
			mcp.Description("逗号分隔的关键字列表，将同时匹配descr和netname字段"),
		),
	)
	s.AddTool(keywordLookupTool, keywordLookupHandler(db))

	if err := server.ServeStdio(s); err != nil {
		log.Printf("server error: %v", err)
	}
}

func ipLookupHandler(db *sql.DB) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ipStr, err := request.RequireString("ip")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		result, err := lookupByIP(ctx, db, strings.TrimSpace(ipStr))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return mcp.NewToolResultText(result), nil
	}
}

func keywordLookupHandler(db *sql.DB) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		rawKeywords, err := request.RequireString("keywords")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		keywords := sanitizeKeywords(rawKeywords)
		if len(keywords) == 0 {
			return mcp.NewToolResultError("至少提供一个有效关键字"), nil
		}

		result, err := lookupByKeywords(ctx, db, keywords)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		return mcp.NewToolResultText(result), nil
	}
}

func lookupByIP(ctx context.Context, db *sql.DB, ipStr string) (string, error) {
	parsed := net.ParseIP(ipStr)
	if parsed == nil || parsed.To4() == nil {
		return "", fmt.Errorf("无效的IPv4地址: %s", ipStr)
	}

	ipInt, err := stringIPToInt(ipStr)
	if err != nil {
		return "", err
	}

	const query = `select inetnum, netname, country, descr, status, "last-modified"
from ipseg
where start <= ? and end >= ?
order by end - start asc
limit 1`

	var inetnum, netname, country, descr, status, lastModified string

	err = db.QueryRowContext(ctx, query, ipInt, ipInt).Scan(
		&inetnum, &netname, &country, &descr, &status, &lastModified,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("未查询到 IP %s 的记录", ipStr)
	}
	if err != nil {
		return "", fmt.Errorf("查询IP信息失败: %w", err)
	}

	var builder strings.Builder
	builder.WriteString("IP: ")
	builder.WriteString(ipStr)
	builder.WriteString("\nIP段: ")
	builder.WriteString(inetnum)
	builder.WriteString("\n名称: ")
	builder.WriteString(netname)
	builder.WriteString("\n描述: ")
	builder.WriteString(descr)
	builder.WriteString("\n国家: ")
	builder.WriteString(country)
	builder.WriteString("\n状态: ")
	builder.WriteString(status)
	builder.WriteString("\n最后修改: ")
	builder.WriteString(lastModified)

	return builder.String(), nil
}

func lookupByKeywords(ctx context.Context, db *sql.DB, keys []string) (string, error) {
	query := buildKeywordQuery(len(keys))

	args := make([]interface{}, 0, len(keys)*2)
	for _, key := range keys {
		args = append(args, "%"+key+"%")
	}
	for _, key := range keys {
		args = append(args, "%"+key+"%")
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("查询关键字失败: %w", err)
	}
	defer rows.Close()

	var builder strings.Builder
	count := 0

	for rows.Next() {
		var inetnum, netname, descr string
		if err := rows.Scan(&inetnum, &netname, &descr); err != nil {
			return "", fmt.Errorf("读取查询结果失败: %w", err)
		}

		count++
		if count > maxKeywordResults {
			break
		}

		fmt.Fprintf(&builder, "序号: %d\nIP段: %s\n名称: %s\n描述: %s\n\n", count, inetnum, netname, descr)
	}

	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("遍历查询结果失败: %w", err)
	}

	if count == 0 {
		return "", fmt.Errorf("未查询到匹配关键字 %v 的结果", keys)
	}

	return strings.TrimRight(builder.String(), "\n"), nil
}

func sanitizeKeywords(value string) []string {
	chunks := strings.Split(value, ",")
	out := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		if trimmed := strings.TrimSpace(chunk); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func buildKeywordQuery(keywordCount int) string {
	if keywordCount <= 0 {
		return ""
	}

	descrConds := make([]string, keywordCount)
	netnameConds := make([]string, keywordCount)
	for i := 0; i < keywordCount; i++ {
		descrConds[i] = "descr like ?"
		netnameConds[i] = "netname like ?"
	}

	var builder strings.Builder
	builder.WriteString("select inetnum, netname, descr from ipseg where (")
	builder.WriteString(strings.Join(descrConds, " and "))
	builder.WriteString(") or (")
	builder.WriteString(strings.Join(netnameConds, " and "))
	builder.WriteString(") limit ")
	builder.WriteString(strconv.Itoa(maxKeywordResults + 1))
	builder.WriteString(";")

	return builder.String()
}

func stringIPToInt(ipString string) (int, error) {
	segments := strings.Split(ipString, ".")
	if len(segments) != 4 {
		return 0, fmt.Errorf("无效的IPv4地址: %s", ipString)
	}

	var ipInt int
	var shift uint = 24
	for _, segment := range segments {
		value, err := strconv.Atoi(segment)
		if err != nil || value < 0 || value > 255 {
			return 0, fmt.Errorf("无效的IPv4地址: %s", ipString)
		}

		ipInt |= value << shift
		shift -= 8
	}

	return ipInt, nil
}

func resolveDBPath() (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取可执行文件路径失败: %w", err)
	}

	dir := filepath.Dir(execPath)
	return filepath.Join(dir, "IP.db"), nil
}
