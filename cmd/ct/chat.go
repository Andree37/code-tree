package main

// Chats: talk to an agent about one node. The transcript lives in
// .tree/chats/<id>.md as plain text; the agent is whatever `chat =` in
// .tree/config runs (prompt on stdin, reply on stdout).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const defaultChat = `claude -p --allowedTools "Read,Grep,Glob,Bash(ct:*)"`

func chatPath(tree, id string) string    { return filepath.Join(tree, "chats", id+".md") }
func pendingPath(tree, id string) string { return filepath.Join(tree, "chats", id+".pending") }

func appendChat(tree, id, who, text string) {
	_ = os.MkdirAll(filepath.Join(tree, "chats"), 0o755)
	f, err := os.OpenFile(chatPath(tree, id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		die("%v", err)
	}
	defer f.Close()
	fmt.Fprintf(f, "@@ %s %s\n%s\n\n", strings.ReplaceAll(flat(who), " ", "_"), now(), chatText(text))
}

// chatText keeps a message from starting a new one: "@@ " opens a message, so
// a line of the text that begins with it is indented.
func chatText(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for k, l := range lines {
		if strings.HasPrefix(l, "@@") {
			lines[k] = " " + l
		}
	}
	return strings.Join(lines, "\n")
}

func readChat(tree, id string) string {
	data, _ := os.ReadFile(chatPath(tree, id))
	return string(data)
}

func cmdChat(a *Args) {
	need(a, 1, "chat")
	tree := requireTree()
	g := load(tree)
	i := g.resolve(a.Pos[0])
	if len(a.Pos) == 1 {
		fmt.Print(readChat(tree, i))
		return
	}
	cfg := loadConfig(tree)
	agent := cfg.ChatName
	appendChat(tree, i, who(a), strings.Join(a.Pos[1:], " "))
	_ = os.WriteFile(pendingPath(tree, i), []byte(now()), 0o644)
	defer os.Remove(pendingPath(tree, i))

	prompt := protocol + "\n\n---\nYou are " + agent + ", chatting with the human about one node of this project's code-tree graph.\n" +
		"You are in the code repo. You may read code, and run `ct` commands to update the tree (nodes you add are proposals).\n" +
		"Reply to the human's last message. Be concise and concrete.\n\n## The node\n" + g.showText(i) +
		"\n## Conversation so far\n" + readChat(tree, i)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cfg.Chat)
	cmd.Dir = cfg.Code
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = helperEnv("CT_TREE="+tree, "CT_AGENT="+agent)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	var out []byte
	err := runGroup(cmd, func() error { var e error; out, e = cmd.Output(); return e })
	reply := strings.TrimSpace(string(out))
	if err != nil {
		reply = fmt.Sprintf("(agent failed: %v) %s\n%s", err, lastLines(stderr.String(), 5), reply)
	}
	if reply == "" {
		reply = "(no reply)"
	}
	appendChat(tree, i, agent, reply)
	fmt.Println(reply)
}
