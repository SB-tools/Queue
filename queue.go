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
	json2 "github.com/disgoorg/disgo/json"
	"github.com/disgoorg/log"
	"github.com/disgoorg/snowflake/v2"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var (
	token         = os.Getenv("SB_QUEUE_TOKEN")
	privateUserID = os.Getenv("SB_REQ_USER_ID")

	// channels
	requestChannelID          = snowflake.ID(1005818150664806480) // TODO change to actual channel id
	logsChannelID             = snowflake.ID(1005863396874399864)
	awaitingReviewChannelID   = snowflake.ID(1005879256557043783)
	awaitingApprovalChannelID = snowflake.ID(1005863036210380911)
	approvedChannelID         = snowflake.ID(1005863016216150056)

	// roles
	awaitingReviewRoleID   = snowflake.ID(1005910399276826665)
	awaitingApprovalRoleID = snowflake.ID(1005910431304532098)

	publicIDRegex      = regexp.MustCompile("\\b[a-f0-9]{64}\\b")
	userInfoURL        = "https://sponsor.ajay.app/api/userInfo?publicUserID=%s&values=[\"userName\",\"segmentCount\",\"ignoredSegmentCount\",\"permissions\"]"
	sbbURL             = "https://sb.ltn.fi/userid/"
	jumplinkTemplate   = fmt.Sprintf("https://discord.com/channels/1005818127474491405/%s/", requestChannelID) // TODO change to SB guild id
	threadNameTemplate = "%d-%s"
	startingMessageID  = snowflake.ID(1005225604066574458)
)

func main() {
	log.SetLevel(log.LevelInfo)
	log.Info("starting the bot...")
	log.Info("disgo version: ", disgo.Version)

	client, err := disgo.New(token,
		bot.WithGatewayConfigOpts(gateway.WithIntents(gateway.IntentGuildMessages, gateway.IntentGuilds, gateway.IntentMessageContent)),
		bot.WithCacheConfigOpts(cache.WithCacheFlags(cache.FlagChannels)),
		bot.WithEventListeners(&events.ListenerAdapter{
			OnGuildMessageCreate:            onMessage,
			OnApplicationCommandInteraction: onCommand,
			OnModalSubmit:                   onModal,
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

	// log the request into the logs and an awaiting channel
	sbbLink := sbbURL + pubID
	embedBuilder := discord.NewEmbedBuilder()
	embedBuilder.SetAuthor("Request "+pubID, sbbLink, author.EffectiveAvatarURL())
	embedBuilder.SetColor(0xf05d0e) // orange
	embedBuilder.SetDescriptionf("**Username**: %s\n**Segment Count**: %d\n**Ignored Segment Count**: %d", userInfo.Username, segmentCount, ignoredSegmentCount)
	embedBuilder.SetTimestamp(time.Now())
	embedMessage := discord.NewMessageCreateBuilder().
		SetEmbeds(embedBuilder.Build()).
		AddActionRow(discord.NewLinkButton("Open sb.ltn.fi", sbbLink),
			discord.NewLinkButton("Jump to message", jumplinkTemplate+messageID.String())).
		Build()
	_, err = client.CreateMessage(logsChannelID, embedMessage)
	if err != nil {
		log.Error("error while sending embed to the logs channel: ", err)
	}

	var awaitingChannelID snowflake.ID
	messageBuilder.SetContentf("Hi %s. Thank your for your interest in contributing to SponsorBlock!\n\n", author.Mention())
	if segmentCount == 0 || segmentCount == ignoredSegmentCount {
		messageBuilder.Content += "You have no submissions on record. If your message doesn't contain a link to a video and timings you want to submit, " +
			"make sure you post the information **into this thread**/**edit your message if you're on Matrix!**"
		awaitingChannelID = awaitingReviewChannelID
	} else {
		messageBuilder.Content += "It looks like you already meet the minimum requirements for permission to submit."
		awaitingChannelID = awaitingApprovalChannelID
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
		threadID := thread.ID()
		_, err = client.CreateMessage(threadID, messageBuilder.Build()) // send pre-built response to the thread
		if err != nil {
			log.Errorf("error while sending pre-built message to thread %d: ", threadID, err)
		}
		starter, err := client.CreateMessage(awaitingChannelID, embedMessage)
		if err != nil {
			log.Error("error while sending embed to an awaiting channel: ", err)
		}
		starterID := starter.ID
		awaitingThread, err := client.CreateThreadFromMessage(awaitingChannelID, starterID, discord.ThreadCreateFromMessage{
			Name:                fmt.Sprintf(threadNameTemplate, threadID, pubID),
			AutoArchiveDuration: discord.AutoArchiveDuration3d,
		})
		if err != nil {
			log.Errorf("error while creating thread from message %d: ", starterID, err)
		} else {
			_, err = client.CreateMessage(awaitingThread.ID(), discord.NewMessageCreateBuilder().
				SetContentf("<@&%d>", ternary(awaitingChannelID == awaitingReviewChannelID, awaitingReviewRoleID, awaitingApprovalRoleID)).
				Build())
		}
	} else {
		_, err = client.CreateMessage(channelID, messageBuilder.SetMessageReferenceByID(messageID).Build()) // reply to the message with the pre-built response
		if err != nil {
			log.Error("error while sending pre-built message: ", err)
		}
	}
}

func onCommand(event *events.ApplicationCommandInteractionCreate) {
	data := event.SlashCommandInteractionData()
	name := data.CommandName()
	if name == "approve" {
		channel, _ := event.Channel()
		if channel.Type() != discord.ChannelTypeGuildPublicThread {
			_ = event.CreateMessage(discord.NewMessageCreateBuilder().
				SetContent("Only run this command in a thread targeting a specific public ID.").
				SetEphemeral(true).
				Build())
			return
		}
		_ = event.CreateModal(discord.NewModalCreateBuilder().
			SetCustomID(discord.CustomID(channel.Name())).
			SetTitle("Would you like to add a comment?").
			AddActionRow(discord.NewParagraphTextInput("comment", "Your comment")).
			Build())
	}
}

func onModal(event *events.ModalSubmitInteractionCreate) {
	data := event.Data
	comment := data.Text("comment")
	client := event.Client().Rest()
	id := data.CustomID
	split := strings.Split(id.String(), "-")
	requestThreadID, _ := snowflake.Parse(split[0])
	messageBuilder := discord.NewMessageCreateBuilder()
	if comment != "" {
		_, err := client.CreateMessage(requestThreadID, messageBuilder.SetContent(comment).Build())
		if err != nil {
			log.Errorf("error while sending comment to thread %d: ", requestThreadID, err)
			_ = event.CreateMessage(messageBuilder.
				SetContentf("There was an error while sending the comment: %s", err).
				SetEphemeral(true).
				Build())
		} else {
			_ = event.CreateMessage(messageBuilder.
				SetContent("Comment sent.").
				SetEphemeral(true).
				Build())
		}
	} else {
		_ = event.CreateMessage(messageBuilder.
			SetContent("No comment was provided").
			SetEphemeral(true).
			Build())
	}

	// send approval message
	_, err := client.CreateMessage(requestThreadID, messageBuilder.
		SetContentf("Your request has been approved! Thanks for your patience.").
		Build())
	if err != nil {
		log.Errorf("error while sending the approval message to thread %d: ", requestThreadID, err)
	}

	// archive original thread
	_, err = client.UpdateChannel(requestThreadID, discord.GuildThreadUpdate{
		Archived: json2.NewPtr(true),
	})
	if err != nil {
		log.Errorf("error while archiving original thread %d: ", requestThreadID, err)
	}

	// archive awaiting thread
	awaitingThreadID := event.ChannelID()
	_, err = client.UpdateChannel(awaitingThreadID, discord.GuildThreadUpdate{
		Archived: json2.NewPtr(true),
	})
	if err != nil {
		log.Errorf("error while archiving awaiting thread %d: ", awaitingThreadID, err)
	}

	// delete awaiting message
	channel, _ := event.Channel()
	parent := *channel.(discord.GuildThread).ParentID()
	err = client.DeleteMessage(parent, awaitingThreadID)
	if err != nil {
		log.Errorf("error while deleting awaiting message %d: ", awaitingThreadID, err)
	}

	// log the approved request
	pubID := split[1]
	_, err = client.CreateMessage(approvedChannelID, messageBuilder.
		SetContentf("User ID: **%s**\nApproved by %s on <t:%d>.", pubID, event.User().Mention(), time.Now().Unix()).
		AddActionRow(discord.NewLinkButton("Open sb.ltn.fi", sbbURL+pubID)).
		SetAllowedMentions(&discord.AllowedMentions{}).
		Build())
}

func ternary[T any](exp bool, ifCond T, elseCond T) T { // https://github.com/aidenwallis/go-utils/blob/main/utils/ternary.go
	if exp {
		return ifCond
	}
	return elseCond
}

type UserInfo struct {
	Username            string `json:"userName"`
	SegmentCount        int    `json:"segmentCount"`
	IgnoredSegmentCount int    `json:"ignoredSegmentCount"`
	Permissions         struct {
		Sponsor bool `json:"sponsor"`
	} `json:"permissions"`
}
