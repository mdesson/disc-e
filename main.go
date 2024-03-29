package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"
)

const (
	ZackID = "144628264583954433"
)

type Config struct {
	DiscordToken string `json:"discordToken"`
	OpenAIKey    string `json:"openAIKey"`
	SpecialUser  string `json:"specialUser"`
	SpeicalReply string `json:"specialReply"`
}

type ImageRequest struct {
	ID       string
	Prompt   string
	AuthorID string
	Guild    *discordgo.Guild
	Channel  *discordgo.Channel
}

type ImageResponse struct {
	Created int                 `json:"created"`
	Data    []map[string]string `json:"data"`
}

var config Config

func main() {
	err := loadConfig(&config)
	if err != nil {
		log.Fatal(err)
	}

	discord, err := discordgo.New("Bot " + config.DiscordToken)
	if err != nil {
		log.Fatal(err)
	}

	discord.AddHandler(onMessageHandler)
	discord.AddHandler(onEmojiAddHandler)

	err = discord.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer discord.Close()

	fmt.Println("DISC-E is listening. Press CTRL-C to exit")

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)

	select {
	case <-sc:
		fmt.Println("\nExiting...")
	}
}

func loadConfig(config *Config) error {
	jsonFile, err := os.Open("config.json")
	if err != nil {
		return err
	}

	jsonBytes, err := ioutil.ReadAll(jsonFile)
	if err != nil {
		return err
	}

	if err = json.Unmarshal(jsonBytes, &config); err != nil {
		return err
	}

	return nil
}

func onEmojiAddHandler(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	// Get original message
	m, err := s.ChannelMessage(r.ChannelID, r.MessageID)
	if err != nil {
		fmt.Printf("[%s] Error on getting message %v\n", r.MessageID, err)
		return
	}

	// Ignore user messages, reactions from the bot, and messages with no replies
	if m.Author.ID != s.State.User.ID || r.MessageReaction.UserID == s.State.User.ID || m.ReferencedMessage == nil {
		return
	}

	if r.Emoji.Name != "🔁" {
		return
	}

	// Find the original message requesting the image
	m = m.ReferencedMessage
	content := strings.ToLower(m.Content)
	for {
		hasNullReply := m.ReferencedMessage == nil
		fromBot := m.Author.ID == s.State.User.ID
		hasCorrectFormat := len(content) >= 7 && content[:7] == "/dalle "

		if fromBot {
			if hasNullReply {
				// Ignore bot messages with no replies
				return
			} else {
				// Bot message has a reply, keep searching
				m = m.ReferencedMessage
				content = strings.ToLower(m.Content)
				continue
			}
		} else {
			if hasCorrectFormat {
				// Message from the user in the correct format, we found our message
				break
			} else {
				// Irrelevant user message, ignore
				return
			}
		}
	}

	prompt := strings.TrimSpace(content[7:])
	guild, _ := s.Guild(m.GuildID)
	channel, _ := s.Channel(m.ChannelID)

	if prompt == "help" {
		return
	}

	fmt.Printf("[%v] Sending variation for prompt: %s\n", r.MessageID, prompt)
	setStatus(s, r.ChannelID, r.MessageID, "🤖")

	imgReq := ImageRequest{
		ID:       m.ID,
		Prompt:   prompt,
		AuthorID: m.Author.ID,
		Guild:    guild,
		Channel:  channel,
	}

	imgURL, err := fetchImage(&imgReq)
	if err != nil {
		fmt.Printf("[%s] Error on getting message %v\n", r.MessageID, err)
		return
	}
	reply, err := s.ChannelMessageSendReply(channel.ID, imgURL, m.Reference())
	if err != nil {
		swapStatus(s, r.ChannelID, r.MessageID, "🤖", "❌")
		fmt.Printf("[%s] %v\n", imgReq.ID, err)
		return
	}
	swapStatus(s, r.ChannelID, r.MessageID, "🤖", "✅")
	setStatus(s, reply.ChannelID, reply.ID, "🔁")

	fmt.Printf("[%s] Sent variation\n", imgReq.ID)
}

func onMessageHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	content := strings.ToLower(m.Content)
	if len(content) <= 7 || content[:7] != "/dalle " || m.Author.ID == s.State.User.ID {
		return
	}

	prompt := strings.TrimSpace(content[7:])
	guild, _ := s.Guild(m.GuildID)
	channel, _ := s.Channel(m.ChannelID)

	imgReq := ImageRequest{
		ID:       m.ID,
		Prompt:   prompt,
		AuthorID: m.Message.Author.ID,
		Guild:    guild,
		Channel:  channel,
	}

	// display help message if relevant
	if prompt == "help" {
		s.ChannelMessageSend(imgReq.Channel.ID, "Type `/dalle` with some words to get an image! (`/dalle help` to display this message)\n🔁 = Click this to try again for a better picture\n🤖 = AI is working on it\n✅ = Done! I've sent your nightmare fuel\n❌ = It didn't work for some reason")
		return
	}

	// update status to show that AI is working on the request
	err := s.MessageReactionAdd(imgReq.Channel.ID, imgReq.ID, "🤖")
	if err != nil {
		fmt.Printf("[%s] %s", imgReq.ID, err)
		return
	}

	// if SpecialUser is set, send them their special reply
	if config.SpecialUser != "" && config.SpecialUser == imgReq.AuthorID {
		_, err := s.ChannelMessageSendReply(channel.ID, config.SpeicalReply, m.Reference())
		if err != nil {
			fmt.Printf("[%s] %v\n", imgReq.ID, err)
			swapStatus(s, imgReq.Channel.ID, imgReq.ID, "🤖", "❌")
			return
		}
	}

	// http request to AI backend
	imgURL, err := fetchImage(&imgReq)
	if err != nil {
		fmt.Printf("[%s] %s\n", imgReq.ID, err)
		swapStatus(s, imgReq.Channel.ID, imgReq.ID, "🤖", "❌")
		return
	}

	// send to channel
	reply, err := s.ChannelMessageSendReply(channel.ID, imgURL, m.Reference())
	if err != nil {
		fmt.Printf("[%s] %v\n", imgReq.ID, err)
		swapStatus(s, imgReq.Channel.ID, imgReq.ID, "🤖", "❌")
		return
	}

	swapStatus(s, imgReq.Channel.ID, imgReq.ID, "🤖", "✅")
	setStatus(s, reply.ChannelID, reply.ID, "🔁")
	fmt.Printf("[%s] Successfully sent message to channel\n", imgReq.ID)
}

func fetchImage(imgReq *ImageRequest) (string, error) {
	fmt.Printf("[%s] Fetching images for prompt %s\n", imgReq.ID, imgReq.Prompt)

	// Create http request
	url := "https://api.openai.com/v1/images/generations"
	jsonStr := fmt.Sprintf(`{"prompt": "%s", "n": 1, "size": "512x512"}`, imgReq.Prompt)
	jsonBytes := []byte(jsonStr)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return "", err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", config.OpenAIKey))

	// Make Request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	// Extract URL from response
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	resp.Body.Close()

	var r ImageResponse
	err = json.Unmarshal(b, &r)
	if err != nil {
		return "", err
	}

	imgURL := r.Data[0]["url"]

	return imgURL, nil
}

func swapStatus(s *discordgo.Session, channelID string, messageID string, oldEmoji string, newEmoji string) error {
	err := s.MessageReactionRemove(channelID, messageID, oldEmoji, "@me")
	if err != nil {
		return err
	}
	err = s.MessageReactionAdd(channelID, messageID, newEmoji)
	if err != nil {
		return err
	}
	return nil
}

func setStatus(s *discordgo.Session, channelID string, messageID string, emoji string) error {
	err := s.MessageReactionAdd(channelID, messageID, emoji)
	if err != nil {
		return err
	}
	return nil
}
