package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nlopes/slack"
)

type Config struct {
	Token string `json:"token"`
}

func (cfg *Config) Load(file string) {
	f, err := os.Open(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "%v", err)
	}
	defer f.Close()
	bytes, err := ioutil.ReadAll(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "%v", err)
	}
	json.Unmarshal(bytes, cfg)
}

func (cfg *Config) Save(file string) {
	f, err := os.Create(file)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.Encode(cfg)
}

const (
	// https://api.slack.com/docs/message-formatting
	maxMessageLen = 4000
)

var (
	token       string
	configPath  string
	channel     string
	processList map[string]*os.Process
	mutex       sync.Mutex
)

func init() {
	flag.StringVar(&token, "token", "", "")
	flag.StringVar(&channel, "channel", "", "")

	homeEnv := "HOME"
	if runtime.GOOS == "windows" {
		homeEnv = "USERPROFILE"
	}

	configPath = filepath.Join(os.Getenv(homeEnv), ".slack-cmd/token.json")
	processList = make(map[string]*os.Process)
}

func op(channelID string, text string, rtm *slack.RTM, api *slack.Client) {
	text = html.UnescapeString(text)
	go func(text string) {
		cmd := exec.Command("sh", "-c", fmt.Sprintf("source ~/.bash_profile && %s", text))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		out, _ := cmd.StdoutPipe()
		cmd.Stderr = nil
		err := cmd.Start()
		if err != nil {
			log.Printf("%v\n", err)
			return
		}

		contentFromLines := func(lines []string) string {
			return "```\n" + strings.Join(lines, "\n") + "\n```"
		}

		lines := []string{}
		hostname, _ := os.Hostname()
		lines = append(lines, fmt.Sprintf("execute \"%s\" on %s", text, hostname))
		params := slack.NewPostMessageParameters()
		_, timestamp, _ := api.PostMessage(channelID, contentFromLines(lines), params)
		usedTimestamps := []string{timestamp}

		if cmd.Process != nil {
			mutex.Lock()
			processList[timestamp] = cmd.Process
			mutex.Unlock()
		}

		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			line := scanner.Text()
			msg := fmt.Sprintf("[%d] %s", cmd.Process.Pid, line)
			lines = append(lines, msg)

			content := contentFromLines(lines)
			if len(content) > maxMessageLen {
				content = contentFromLines(lines[0 : len(lines)-1])
				api.UpdateMessage(channelID, timestamp, content)
				lines = []string{msg}
				_, timestamp, _ = api.PostMessage(channelID, contentFromLines(lines), params)

				usedTimestamps = append(usedTimestamps, timestamp)
				mutex.Lock()
				processList[timestamp] = cmd.Process
				mutex.Unlock()
			} else {
				api.UpdateMessage(channelID, timestamp, content)
			}
		}
		cmd.Wait()

		lines = append(lines, fmt.Sprintf("terminated with %v", cmd.ProcessState.Sys()))
		api.UpdateMessage(channelID, timestamp, "```\n"+strings.Join(lines, "\n")+"\n```")

		mutex.Lock()
		for _, t := range usedTimestamps {
			delete(processList, t)
		}
		mutex.Unlock()
	}(text)
}

func run(rtm *slack.RTM, api *slack.Client) {
	start := float64(time.Now().Unix())
	for {
		select {
		case msg := <-rtm.IncomingEvents:
			switch event := msg.Data.(type) {
			case *slack.MessageEvent:
				timestamp, _ := strconv.ParseFloat(event.Timestamp, 64)
				if event.SubType == "message_replied" {
					target_timestamp := event.SubMessage.Timestamp
					mutex.Lock()
					process := processList[target_timestamp]
					if process != nil {
						// "Killing a child process and all of its children in Go"
						// https://medium.com/@felixge/killing-a-child-process-and-all-of-its-children-in-go-54079af94773#.s092vaa8w
						syscall.Kill(-process.Pid, syscall.SIGKILL)
					}
					mutex.Unlock()
				} else if event.SubType == "" && start < timestamp {
					text := event.Msg.Text
					ch, _ := api.GetChannelInfo(event.Channel)
					if ch != nil && ch.Name == channel && text != "quit" {
						op(event.Channel, text, rtm, api)
					}
				}
			case *slack.InvalidAuthEvent:
				log.Println("Invalid credentials")
				break
			}
		}
	}
}

func main() {
	flag.Parse()

	var cfg Config
	cfg.Load(configPath)

	if token != "" {
		cfg.Token = token
		cfg.Save(configPath)
	}

	if channel == "" {
		panic(fmt.Errorf("channel name is empty"))
	}

	api := slack.New(cfg.Token)

	rtm := api.NewRTM()
	go rtm.ManageConnection()
	run(rtm, api)
}
