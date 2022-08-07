package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/log"
	"github.com/disgoorg/snowflake/v2"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"
)

var (
	token             = os.Getenv("SB_QUEUE_TOKEN")
	requestChannelID  = snowflake.ID(1005818150664806480) // TODO change to actual channel id
	logsChannelID     = snowflake.ID(1005863396874399864)
	publicIDRegex     = regexp.MustCompile("\\b[a-f0-9]{64}\\b")
	userInfoURL       = "https://sponsor.ajay.app/api/userInfo?publicUserID=%s&values=[\"userName\",\"segmentCount\",\"ignoredSegmentCount\",\"permissions\"]"
	sbbURL            = "https://sb.ltn.fi/userid/"
	jumplinkTemplate  = fmt.Sprintf("https://discord.com/channels/1005818127474491405/%s/", requestChannelID) // TODO change to SB guild id
	startingMessageID = snowflake.ID(1005225604066574458)
)

func main() {
	log.SetLevel(log.LevelInfo)
	log.Info("starting the bot...")
	log.Info("disgo version: ", disgo.Version)

	client, err := disgo.New(token,
		bot.WithGatewayConfigOpts(gateway.WithIntents(gateway.IntentGuildMessages, gateway.IntentGuilds, gateway.IntentMessageContent)),
		bot.WithCacheConfigOpts(cache.WithCacheFlags(cache.FlagChannels)),
		bot.WithEventListeners(&events.ListenerAdapter{
			OnGuildMessageCreate: onMessage,
		}))

	if err != nil {
		log.Fatal("error while building disgo: ", err)
		return
	}

	defer client.Close(context.TODO())

	err = client.OpenGateway(context.TODO())
	if err != nil {
		log.Fatalf("error while connecting to the gateway: %s", err)
		return
	}

	log.Info("SB queue started")

	s := make(chan os.Signal, 1)
	signal.Notify(s, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-s
}

func onMessage(event *events.GuildMessageCreate) {
	channelID := event.ChannelID
	message := event.Message
	author := message.Author
	if channelID != requestChannelID || author.Bot {
		return
	}
	pubID := publicIDRegex.FindString(message.Content)
	if pubID == "" {
		return
	}
	response, err := http.Get(fmt.Sprintf(userInfoURL, pubID))
	if err != nil {
		log.Error("error while getting user info: ", err)
		return
	}
	code := response.StatusCode
	if code != 200 {
		log.Warn("received code %d while getting user info.", code)
		return
	}
	closer := response.Body
	body, err := io.ReadAll(closer)
	err = closer.Close()
	if err != nil {
		log.Error("error while reading response body: ", err)
		return
	}
	var userInfo UserInfo
	err = json.Unmarshal(body, &userInfo)
	if err != nil {
		log.Error("error while unmarshalling user info: ", err)
		return
	}
	messageBuilder := discord.NewMessageCreateBuilder()
	client := event.Client().Rest()
	messageID := event.MessageID
	if userInfo.Permissions.Sponsor { // check if the user already has perms
		_, _ = client.CreateMessage(channelID, messageBuilder.
			SetContent("You already have permission to submit.").
			SetMessageReferenceByID(messageID).
			Build())
		return
	}
	segmentCount := userInfo.SegmentCount
	ignoredSegmentCount := userInfo.IgnoredSegmentCount

	// log the request into a channel
	embedBuilder := discord.NewEmbedBuilder()
	embedBuilder.SetAuthor("Request "+pubID, sbbURL+pubID, author.EffectiveAvatarURL())
	embedBuilder.SetDescriptionf("**Username**: %s\n**Segment Count**: %d\n**Ignored Segment Count**: %d", userInfo.Username, segmentCount, ignoredSegmentCount)
	embedBuilder.SetTimestamp(time.Now())
	_, _ = client.CreateMessage(logsChannelID, discord.NewMessageCreateBuilder().
		SetEmbeds(embedBuilder.Build()).
		AddActionRow(discord.NewLinkButton("Jump to message", jumplinkTemplate+messageID.String())).
		Build())

	messageBuilder.SetContentf("Hi %s. Thank your for your interest in contributing to SponsorBlock!\n\n", author.Mention())
	if segmentCount == 0 || segmentCount == ignoredSegmentCount {
		messageBuilder.Content += "You have no submissions on record. If your message doesn't contain a link to a video and timings you want to submit, " +
			"make sure you post the information **into this thread**/**edit your message if you're on Matrix!**"
	} else {
		messageBuilder.Content += "It looks like you already meet the minimum requirements for permission to submit."
	}
	messageBuilder.Content += "\n\nAll you need to do now is **wait for our review** and we will get back to you **as soon as possible!**"
	if message.WebhookID == nil {
		thread, er := client.CreateThreadFromMessage(channelID, messageID, discord.ThreadCreateFromMessage{
			Name:                pubID,
			AutoArchiveDuration: discord.AutoArchiveDuration3d,
		})
		if er != nil {
			log.Error("error while creating thread: ", er)
			return
		}
		_, err = client.CreateMessage(thread.ID(), messageBuilder.Build())
	} else {
		_, err = client.CreateMessage(channelID, messageBuilder.SetMessageReferenceByID(messageID).Build())
	}
	if err != nil {
		log.Error("error while creating message: ", err)
		return
	}
}

type UserInfo struct {
	Username            string `json:"userName"`
	SegmentCount        int    `json:"segmentCount"`
	IgnoredSegmentCount int    `json:"ignoredSegmentCount"`
	Permissions         struct {
		Sponsor bool `json:"sponsor"`
	} `json:"permissions"`
}
