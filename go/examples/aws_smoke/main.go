// One real round trip on AWS: create, run, stream, list, reattach, destroy.
// The machine has a 30-minute TTL, so it terminates itself if this is interrupted.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"example.com/nepho"
	"example.com/nepho/aws"
)

func main() {
	ctx := context.Background()
	p, err := aws.New(ctx, aws.Options{KeyPath: os.Getenv("NEPHO_KEY")})
	if err != nil {
		log.Fatal(err)
	}
	t := time.Now()
	m, err := p.Create(ctx, nepho.Spec{OS: "ubuntu-24.04", TTL: 30 * time.Minute, Setup: nepho.Setup{
		Packages: []string{"jq"}, Files: map[string]string{"/etc/nepho-test": "hello\n"},
		Script: "echo done > /root/marker"}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("created", m.ID, time.Since(t).Round(time.Second))
	id := m.ID
	defer func() {
		t := time.Now()
		if err := p.Destroy(ctx, id); err != nil {
			log.Print("destroy: ", err)
		}
		fmt.Println("destroyed", time.Since(t).Round(time.Second))
		ms, _ := p.List(ctx)
		for _, i := range ms {
			fmt.Println("after:", i.ID, i.State)
		}
	}()
	if err := checks(ctx, p, m); err != nil {
		log.Print(err)
	}
}

func checks(ctx context.Context, p *nepho.Provider, m *nepho.Machine) error {
	r, err := m.Run(ctx, nepho.Sh("uname -a; cat /etc/nepho-test; jq --version; sudo cat /root/marker; whoami"), "", nil)
	if err != nil {
		return err
	}
	fmt.Println(r.Code, r.Stdout, r.Stderr)
	pr, err := m.Execute(ctx, []string{"cat"}, nil)
	if err != nil {
		return err
	}
	fmt.Fprintln(pr.Stdin, "stream ok")
	line, _ := bufio.NewReader(pr.Stdout).ReadString('\n')
	fmt.Println("echo:", strings.TrimSpace(line))
	pr.Stdin.Close()
	code, err := pr.Wait()
	fmt.Println("cat exit", code, err)
	if r, err = m.Run(ctx, []string{"id", "-un"}, "", &nepho.ExecOptions{User: "root"}); err != nil {
		return err
	}
	fmt.Println("as root:", strings.TrimSpace(r.Stdout))
	_ = m.Detach()
	ms, err := p.List(ctx)
	if err != nil {
		return err
	}
	for _, i := range ms {
		fmt.Println("list:", i.ID, i.State, i.Address)
	}
	if m, err = p.Attach(ctx, m.ID); err != nil {
		return err
	}
	if r, err = m.Run(ctx, []string{"uptime"}, "", nil); err != nil {
		return err
	}
	fmt.Println("reattached:", strings.TrimSpace(r.Stdout))
	return nil
}
