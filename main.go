package main

import (
	"log"

	"github.com/A-UNDERSCORE-D/goplay-irc/internal/bot"
	"github.com/pelletier/go-toml"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	c := &bot.BotConfig{}
	res, err := toml.LoadFile("./config.toml")
	if err != nil {
		log.Fatal(err)
	}

	res.Unmarshal(c)
	b := bot.New(c)

	b.Run()
}
