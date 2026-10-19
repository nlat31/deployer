package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"deployer/internal/model"
	"deployer/internal/packages"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "reset-admin" {
		if err := resetAdmin(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		fmt.Println("管理员密码已重置为内置初始密码，验证码绑定已解除。")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "restore-backup" {
		if err := restoreBackup(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		fmt.Println("备份已恢复到新数据目录。")
		return
	}

	if len(os.Args) < 2 || os.Args[1] != "check-package" {
		fmt.Fprintln(os.Stderr, "用法: deployerctl check-package --app sample --version 1.0.0 --arch amd64 package.tar.gz")
		os.Exit(2)
	}
	f := flag.NewFlagSet("check-package", flag.ExitOnError)
	app := f.String("app", "", "App 标识")
	version := f.String("version", "", "版本")
	arch := f.String("arch", "amd64", "CPU 架构")
	f.Parse(os.Args[2:])
	if f.NArg() != 1 {
		log.Fatal("请选择一个包文件")
	}
	tmp, err := os.MkdirTemp("", "deployer-check-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	v := model.Version{AppID: *app, Version: *version, OS: "linux", Arch: *arch}
	if _, err = packages.Extract(f.Arg(0), filepath.Join(tmp, "package"), v); err != nil {
		log.Fatal(err)
	}
	digest, err := packages.Digest(f.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("包检查通过\nApp: %s\n版本: %s\n平台: linux/%s\nSHA-256: %s\n", *app, *version, *arch, digest)
}
