package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	gim "github.com/ozankasikci/go-image-merge"
)

type Config struct {
	DiscordToken string   `json:"discordToken"`
	ServerNames  []string `json:"serverNames"`
	Parallelism  int      `json:"parallelism"`
	DalleRetries int      `json:"dalleRetries"`
}

type ImageRequest struct {
	ID       string
	Prompt   string
	Author   string
	Guild    *discordgo.Guild
	Channel  *discordgo.Channel
	Duration time.Duration
}

type ImageResponse struct {
	Images []string `json:"images"`
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

func onMessageHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	content := strings.ToLower(m.Content)
	if len(content) <= 7 || content[:7] != "/dalle " || m.Author.ID == s.State.User.ID {
		return
	}

	prompt := strings.TrimSpace(content[7:])
	guild, _ := s.Guild(m.GuildID)
	channel, _ := s.Channel(m.ChannelID)

	imgReq := ImageRequest{
		ID:      m.ID,
		Prompt:  prompt,
		Author:  m.Message.Author.Username,
		Guild:   guild,
		Channel: channel,
	}

	// display help message if relevant
	if prompt == "help" {
		s.ChannelMessageSend(imgReq.Channel.ID, "Type `/dalle` with some words to get an image! (`/dalle help` to display this message)\n🤖 = AI is working on it\n🛠️ = I'm preparing your images\n✅ = Done! I've sent your nightmare fuel\n❌ = It didn't work for some reason")
		return
	}

	// update status to show that AI is working on the request
	err := s.MessageReactionAdd(imgReq.Channel.ID, imgReq.ID, "🤖")
	if err != nil {
		fmt.Printf("[%s] %s", imgReq.ID, err)
		return
	}

	// http request to AI backend
	base64Images, err := fetchImages(&imgReq)
	if err != nil {
		fmt.Printf("[%s] %s", imgReq.ID, err)
		setStatus(s, imgReq, "🤖", "❌")
		return
	}

	// convert base64 slice into one image
	setStatus(s, imgReq, "🤖", "🛠️")

	err = base64ToImage(base64Images, &imgReq)
	if err != nil {
		fmt.Printf("[%s] %s", imgReq.ID, err)
		setStatus(s, imgReq, "🛠️", "❌")
		return
	}

	// open file and delete it when done
	f, err := os.Open(imgReq.ID + ".jpg")
	if err != nil {
		fmt.Printf("[%s] %v\n", imgReq.ID, err)
		setStatus(s, imgReq, "🛠️", "❌")
		return
	}
	defer f.Close()
	defer os.Remove(imgReq.ID + ".jpg")

	// send to channel
	_, err = s.ChannelFileSendWithMessage(channel.ID, fmt.Sprintf("*%s*", prompt), imgReq.ID+".jpg", f)
	if err != nil {
		fmt.Printf("[%s] %v\n", imgReq.ID, err)
		setStatus(s, imgReq, "🛠️", "❌")
		return
	}

	setStatus(s, imgReq, "🛠️", "✅")
	fmt.Printf("[%s] Successfully sent message to channel\n", imgReq.ID)
}

func fetchImages(imgReq *ImageRequest) ([]string, error) {
	fmt.Printf("[%s] Fetching images for prompt %s\n", imgReq.ID, imgReq.Prompt)

	// Create http request
	url := "https://bf.dallemini.ai/generate"
	jsonStr := fmt.Sprintf(`{"prompt": "%s"}`, imgReq.Prompt)
	jsonBytes := []byte(jsonStr)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	start := time.Now()

	// try n times (set in config) to fetch images from DALL-E Mini API
	for i := 0; i < config.DalleRetries; i++ {
		resp, err := client.Do(req)
		if err != nil {
			resp.Body.Close()
			return nil, err
		}

		if resp.StatusCode == 200 {
			imgReq.Duration = time.Now().Sub(start).Truncate(time.Second)

			fmt.Printf("[%s] Success after %d tries (%v)\n", imgReq.ID, i+1, imgReq.Duration)

			b, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return nil, err
			}
			resp.Body.Close()

			var r ImageResponse
			err = json.Unmarshal(b, &r)
			if err != nil {
				return nil, err
			}

			return r.Images, nil
		}
		resp.Body.Close()
	}

	imgReq.Duration = time.Now().Sub(start).Truncate(time.Second)

	return nil, errors.New(fmt.Sprintf("Failed to get images for request (%v)\n", imgReq.Duration))
}

func base64ToImage(base64Images []string, imgReq *ImageRequest) error {
	// temporarily save as file
	for i, base64Img := range base64Images {
		filename := fmt.Sprintf("%s-%d.jpg", imgReq.ID, i)
		unbased, err := base64.StdEncoding.DecodeString(base64Img)
		if err != nil {
			return err
		}

		// create individual image files, clean up when done with them
		err = os.WriteFile(filename, unbased, 0644)
		if err != nil {
			return err
		}
		defer func() {
			err := os.Remove(filename)
			if err != nil {
				fmt.Printf("[%s] %v\n", imgReq.ID, err)
			}
		}()

	}

	// stitch images together
	err := combineImages(imgReq, len(base64Images))
	if err != nil {
		return err
	}

	return nil
}

func combineImages(imgReq *ImageRequest, imageCount int) error {
	grids := []*gim.Grid{}
	for i := 0; i < imageCount; i++ {
		filename := fmt.Sprintf("%s-%d.jpg", imgReq.ID, i)
		grid := gim.Grid{ImageFilePath: filename}
		grids = append(grids, &grid)
	}

	rgba, err := gim.New(grids, 3, 3).Merge()
	if err != nil {
		return err
	}

	file, err := os.Create(imgReq.ID + ".jpg")
	if err != nil {
		return err
	}
	defer file.Close()

	err = jpeg.Encode(file, rgba, &jpeg.Options{Quality: 80})
	if err != nil {
		return err
	}
	fmt.Printf("[%s] Created image\n", imgReq.ID)

	return nil
}

func setStatus(s *discordgo.Session, imgReq ImageRequest, oldEmoji string, newEmoji string) error {
	err := s.MessageReactionRemove(imgReq.Channel.ID, imgReq.ID, oldEmoji, "@me")
	if err != nil {
		return err
	}
	err = s.MessageReactionAdd(imgReq.Channel.ID, imgReq.ID, newEmoji)
	if err != nil {
		return err
	}
	return nil
}
