package bot

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"
	"github.com/haya14busa/goplay"
	"golang.org/x/tools/imports"
)

type BotConfig struct {
	Nick            string `toml:"nick"`
	User            string `toml:"user"`
	RealName        string `toml:"real_name"`
	VersionResponse string `toml:"-"`
	SASLUser        string `toml:"sasl_user"`
	SASLPassword    string `toml:"sasl_password"`
	CommandPrefix   string `toml:"command_prefix"`

	Server       string   `toml:"server"`
	UseTLS       bool     `toml:"use_tls"`
	JoinChannels []string `toml:"join_channels"`
}

type Bot struct {
	config *BotConfig
	irc    *ircevent.Connection

	commands     map[string]*Command
	messageQueue chan ircmsg.Message
}

func New(c *BotConfig) *Bot {
	conn := &ircevent.Connection{
		Server:          c.Server,
		Nick:            c.Nick,
		User:            c.User,
		RealName:        c.RealName,
		SASLLogin:       c.SASLUser,
		SASLPassword:    c.SASLPassword,
		Version:         c.VersionResponse,
		UseTLS:          c.UseTLS,
		UseSASL:         c.SASLPassword != "" && c.SASLUser != "",
		EnableCTCP:      true,
		Debug:           true,
		AllowTruncation: true,
		Log:             log.Default(),
	}

	b := &Bot{config: c, irc: conn, commands: make(map[string]*Command)}
	b.init()
	return b
}

func (b *Bot) init() {
	b.irc.AddCallback("PRIVMSG", b.onPrivmsg)
	b.createCommand("eval", true, b.EvalCmd)
	b.createCommand("play", true, b.runPlayLink)
	b.createCommand("playerrors", true, b.playErrors)
	b.irc.AddConnectCallback(func(_ ircmsg.Message) {
		for _, ch := range b.config.JoinChannels {
			b.irc.Join(ch)
		}
	})
}

func (b *Bot) Run() {
	if err := b.irc.Connect(); err != nil {
		panic(err)
	}
	b.irc.Loop()
}

type (
	ReplyFunc func(string, ...interface{}) error
	Callback  func(args string, reply ReplyFunc)
)

type Command struct {
	name      string
	callback  Callback
	goroutine bool
}

func (b *Bot) createCommand(name string, goroutine bool, callback Callback) {
	b.commands[name] = &Command{
		name:      name,
		callback:  callback,
		goroutine: goroutine,
	}
}

func (b *Bot) onPrivmsg(msg ircmsg.Message) {
	replyTarget := msg.Params[0]
	if replyTarget == b.irc.CurrentNick() {
		replyTarget, _, _ = ircevent.SplitNUH(msg.Prefix)
	}

	msgContent := msg.Params[1]
	if !strings.HasPrefix(msgContent, b.config.CommandPrefix) && !strings.HasPrefix(msgContent, b.irc.CurrentNick()) {
		// Not for us, ignore it
		return
	}

	// its a command, lets parse things out as needed

	var command, rest string
	if strings.HasPrefix(msgContent, b.irc.CurrentNick()) {
		split := strings.SplitN(msgContent, " ", 3)
		command = split[1]
		if len(split) > 2 {
			rest = split[2]
		}
	} else {
		split := strings.SplitN(msgContent, " ", 2)
		command = split[0][len(b.config.CommandPrefix):]
		if len(split) > 1 {
			rest = split[1]
		}

	}

	cmd, cmdExists := b.commands[command]
	if !cmdExists {
		return
	}

	replyFunc := func(s string, a ...interface{}) error {
		if len(a) == 0 {
			return b.irc.Privmsg(replyTarget, s)
		}
		return b.irc.Privmsgf(replyTarget, s, a...)
	}

	if cmd.goroutine {
		go cmd.callback(rest, replyFunc)
	} else {
		cmd.callback(rest, replyFunc)
	}
}

func (b *Bot) EvalCmd(args string, reply ReplyFunc) {
	if strings.TrimSpace(args) == "" {
		reply("Cannot eval empty code")
		return
	}

	builtUp := fmt.Sprintf(`
	package main
	func main() {
		%s
	}
	`, args)
	res, shareLink, err := b.runCode(builtUp, true, true, true)
	if err != nil {
		log.Print("Error while sending request: ", err)
		reply(fmt.Sprintf("Error occurred: %s", err))
	}

	if len(res.Errors) != 0 {
		// Compile failed
		log.Print("Error while running compile: ", res.Errors)
		reply(fmt.Sprintf("Compile failed! %s", strings.TrimSpace(res.Errors)))
		return
	}

	// No errors
	log.Printf("Completed successfully: %s", shareLink)
	if len(res.Events) == 0 {
		reply("Complete, but no prints")
	} else {
		extraInfo := ""
		if len(res.Events) > 1 {
			extraInfo = fmt.Sprintf(" (First line only. %d events returned)", len(res.Events))
		}
		reply("Complete %s%s : %s", shareLink, extraInfo, ExtractFirstLine(res.Events[0].Message))
	}
}

func ExtractFirstLine(s string) string {
	return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
}

var (
	snippetValidRe         = regexp.MustCompile(`[a-zA-Z0-9]{8,}(?:\.go)?`)
	goplaygroundURIValidRE = regexp.MustCompile(`^(?:https?://)?play.golang.org/p/([a-zA-Z0-9]{8,}(?:\.go)?)$`)
)

func snippetIsValid(snippet string) bool {
	return snippetValidRe.MatchString(snippet)
}

func (b *Bot) runCode(code string, doShare, doImports, doFormat bool) (*goplay.Response, string, error) {
	codeBytes := []byte(code)
	var err error
	if doImports || doFormat {
		codeBytes, err = imports.Process("prog.go", codeBytes, &imports.Options{
			Fragment:   false,
			AllErrors:  false,
			Comments:   true,
			TabIndent:  true,
			TabWidth:   8,
			FormatOnly: !doImports,
		})
	}
	if err != nil {
		return nil, "", err
	}

	var share string
	if doShare {
		share = "Unable to create share link"
		s, err := goplay.DefaultClient.Share(bytes.NewReader(codeBytes))
		if err == nil {
			share = s
		} else {
			log.Println(err)
		}
	}

	res, err := goplay.DefaultClient.Compile(bytes.NewReader(codeBytes))
	if err != nil {
		return nil, "", err
	}

	return res, share, nil
}

func extractPlaySnippetID(source string) (string, error) {
	matches := goplaygroundURIValidRE.FindStringSubmatch(source)
	if matches != nil {
		return matches[1], nil
	}

	if snippetIsValid(source) {
		return source, nil
	}

	return "", errors.New("invalid snippet")
}

func downloadPlaySnippet(source string) (string, error) {
	id, err := extractPlaySnippetID(source)
	if err != nil {
		return "", err
	}

	if !strings.HasSuffix(id, ".go") {
		id = id + ".go"
	}
	res, err := http.Get(fmt.Sprintf("%s/p/%s", "https://play.golang.org", id))
	if err != nil {
		log.Print(err)
		return "", err
	}

	switch res.StatusCode {
	case 200:
	case 404:
		return "", errors.New("snippet does not exist")
	default:
		return "", errors.New("unknown error")
	}

	defer res.Body.Close()
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (b *Bot) runPlayLink(args string, reply ReplyFunc) {
	if args == "" {
		reply("Cannot parse an empty link / URL")
		return
	}

	code, err := downloadPlaySnippet(args)
	if err != nil {
		log.Print(err)
	}

	runRes, _, err := b.runCode(code, false, false, false)
	if err != nil {
		log.Println("Unable to start compile", err)
		reply("Unable to start compile: %s", err)
		return
	}

	if len(runRes.Errors) != 0 {
		// Compile failed
		log.Print("Error while running compile: ", runRes.Errors)
		reply(fmt.Sprintf("Compile failed! %s", strings.TrimSpace(runRes.Errors)))
		return
	}

	// No errors
	if len(runRes.Events) == 0 {
		reply("Complete, but no prints")
	} else {
		reply("Complete: %s", ExtractFirstLine(runRes.Events[0].Message))
	}
}

func (b *Bot) playErrors(args string, reply ReplyFunc) {
	if args == "" {
		reply("Cannot parse an empty link / URL")
		return
	}

	code, err := downloadPlaySnippet(args)
	if err != nil {
		log.Print(err)
		reply("Unable to get snippet: %s", err)
		return
	}

	runRes, _, err := b.runCode(code, false, false, false)
	if err != nil {
		log.Println("Unable to start compile", err)
		reply("Unable to start compile: %s", err)
		return
	}

	if len(runRes.Errors) != 0 {
		// Compile failed
		log.Print("Error while running compile: ", runRes.Errors)
		reply(fmt.Sprintf("Errors: %s", strings.TrimSpace(runRes.Errors)))
		return
	}

	reply("No errors in file")
}
