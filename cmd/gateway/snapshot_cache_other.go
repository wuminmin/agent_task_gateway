//go:build !linux

package main

import "os"

func adviseSequentialFile(*os.File) {}

func dropFileCache(*os.File, int64, int64) {}
