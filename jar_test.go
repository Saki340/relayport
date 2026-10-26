package main

import "net/http/cookiejar"

func newJar() *cookiejar.Jar {
	j, _ := cookiejar.New(nil)
	return j
}
