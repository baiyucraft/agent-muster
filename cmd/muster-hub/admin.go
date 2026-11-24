package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/baiyucraft/agent-muster/internal/hub/adminauth"
	"github.com/baiyucraft/agent-muster/internal/hub/postgres"
	"golang.org/x/term"
)

func runAdmin(args []string, stdin *os.File, output io.Writer) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		_, err := fmt.Fprintln(output, "用法: muster-hub admin set --username <用户名>")
		return err
	}
	if len(args) == 0 || args[0] != "set" {
		return errors.New("用法: muster-hub admin set --username <用户名>")
	}
	flags := flag.NewFlagSet("muster-hub admin set", flag.ContinueOnError)
	flags.SetOutput(output)
	username := flags.String("username", "", "管理员用户名，支持字母、数字、点、下划线和连字符")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || !adminauth.ValidUsername(*username) {
		return errors.New("请使用 --username <用户名>，用户名长度为 1 至 64 字节")
	}
	databaseURL := os.Getenv("MUSTER_DATABASE_URL")
	if databaseURL == "" {
		return errors.New("请设置 MUSTER_DATABASE_URL")
	}
	password, err := readAdminPassword(stdin, output)
	if err != nil {
		return err
	}
	passwordHash, err := adminauth.HashPassword(password)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	database, err := postgres.OpenInventory(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("无法连接或初始化管理员数据库 (%T)", err)
	}
	defer database.Close()
	if err := database.SetAdminCredentials(ctx, *username, passwordHash); err != nil {
		return fmt.Errorf("无法保存管理员凭据 (%T)", err)
	}
	_, err = fmt.Fprintln(output, "已设置管理员账号，旧网页登录会话已撤销。")
	return err
}

func readAdminPassword(stdin *os.File, output io.Writer) (string, error) {
	fd := int(stdin.Fd())
	if !term.IsTerminal(fd) {
		// 仅读取首行，保留密码内的空格。允许 LF、CRLF 或文件末尾无换行。
		reader := bufio.NewReaderSize(stdin, adminauth.MaxPasswordBytes+2)
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", errors.New("管理员密码须为 12 至 1024 字节的 UTF-8 文本")
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", errors.New("无法从标准输入读取管理员密码")
		}
		return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"), nil
	}
	fmt.Fprint(output, "管理员密码: ")
	password, err := term.ReadPassword(fd)
	fmt.Fprintln(output)
	if err != nil {
		return "", errors.New("无法读取管理员密码")
	}
	fmt.Fprint(output, "再次输入密码: ")
	confirmation, err := term.ReadPassword(fd)
	fmt.Fprintln(output)
	if err != nil {
		return "", errors.New("无法读取确认密码")
	}
	if string(password) != string(confirmation) {
		return "", errors.New("两次输入的密码不一致")
	}
	return string(password), nil
}
