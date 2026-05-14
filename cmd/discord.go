package cmd

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/disgo/webhook"
	"github.com/disgoorg/snowflake/v2"
)

const (
	colorGood     = 0x00FF00
	colorWarning  = 0xFFAC1C
	colorError    = 0xFF0000
	colorCritical = 0x964B00

	iconGood    = "🟢" // green circle
	iconWarning = "🟡" // yellow circle
	iconError   = "🔴" // red circle
)

type DiscordNotificationService struct {
	webhookURL   string // Full webhook URL
	webhookID    string // Webhook ID (if using ID+Token form)
	webhookToken string // Webhook Token (if using ID+Token form)
	postMutex    *sync.Mutex
}

func formattedTime(t time.Time) string {
	return fmt.Sprintf("<t:%d:R>", t.Unix())
}

func NewDiscordNotificationService(config *DiscordWebhookConfig) (*DiscordNotificationService, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	service := &DiscordNotificationService{
		postMutex: &sync.Mutex{},
	}

	if config.URL != "" {
		service.webhookURL = config.URL
		fmt.Printf("[Discord] Initialized notification service with webhook URL %s\n", config.URL)
	} else {
		service.webhookID = config.ID
		service.webhookToken = config.Token
		fmt.Printf("[Discord] Initialized notification service with webhook ID+Token (ID: %s, Token: %s)\n", config.ID, config.Token)
	}

	return service, nil
}

func getColorForAlertLevel(alertLevel AlertLevel) int {
	switch alertLevel {
	case alertLevelNone:
		return colorGood
	case alertLevelWarning:
		return colorWarning
	case alertLevelCritical:
		return colorCritical
	default:
		return colorError
	}
}

func getCurrentStatsEmbed(stats ValidatorStats, vm *ValidatorMonitor) discord.Embed {
	var uptime string
	var title string
	if vm.FullNode {
		title = vm.Name
	} else {
		if stats.SlashingPeriodUptime == 0 {
			uptime = "N/A"
		} else {
			uptime = fmt.Sprintf("%.02f", stats.SlashingPeriodUptime)
		}

		title = fmt.Sprintf("%s (%s%% up)", vm.Name, uptime)
	}

	var description string
	sentryString := ""

	if vm.Sentries != nil {
		for _, vmSentry := range *vm.Sentries {
			sentryFound := false
			for _, sentryStats := range stats.SentryStats {
				if vmSentry.Name == sentryStats.Name {
					var statusIcon string
					if sentryStats.SentryAlertType == sentryAlertTypeNone {
						statusIcon = iconGood
					} else {
						statusIcon = iconError
					}

					var height string
					if sentryStats.Height == 0 {
						height = "N/A"
					} else {
						height = fmt.Sprint(sentryStats.Height)
					}
					var version string
					if sentryStats.Version == "" {
						version = "N/A"
					} else {
						version = sentryStats.Version
					}

					sentryString += fmt.Sprintf("\n%s **%s** - Height **%s** - Version **%s**", statusIcon, sentryStats.Name, height, version)
					sentryFound = true
					break
				}
			}
			if !sentryFound {
				sentryString += fmt.Sprintf("\n%s **%s** - Height **N/A** - Version **N/A**", iconError, vmSentry.Name)
			}
		}
	}

	recentSignedBlocks := fmt.Sprintf("%s Latest Blocks Signed: **N/A**", iconWarning)

	var latestBlock string
	if stats.Timestamp.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		latestBlock = fmt.Sprintf("%s Height **N/A**", iconError)
	} else {
		var rpcStatusIcon string
		if stats.RPCError {
			rpcStatusIcon = iconError
		} else {
			rpcStatusIcon = iconGood
			if !vm.FullNode {
				var recentSignedBlocksIcon string
				switch level := stats.RecentMissedBlockAlertLevel; {
				case level >= alertLevelHigh:
					recentSignedBlocksIcon = iconError
				case level == alertLevelWarning:
					recentSignedBlocksIcon = iconWarning
				default:
					recentSignedBlocksIcon = iconGood
				}
				recentSignedBlocks = fmt.Sprintf("%s Latest Blocks Signed: **%d/%d**", recentSignedBlocksIcon, vm.RecentBlocksToCheck-stats.RecentMissedBlocks, vm.RecentBlocksToCheck)
			}
		}
		latestBlock = fmt.Sprintf("%s Height **%s** - **%s**", rpcStatusIcon, fmt.Sprint(stats.Height), formattedTime(stats.Timestamp))
	}

	if vm.FullNode {
		description = fmt.Sprintf("%s%s", latestBlock, sentryString)
	} else {
		if stats.Height == stats.LastSignedBlockHeight {
			description = fmt.Sprintf("%s\n%s%s",
				latestBlock, recentSignedBlocks, sentryString)
		} else {
			var lastSignedBlock string
			if stats.LastSignedBlockHeight == -1 {
				lastSignedBlock = fmt.Sprintf("%s Last Signed **N/A**", iconError)
			} else {
				lastSignedBlock = fmt.Sprintf("%s Last Signed **%s** - **%s**", iconError, fmt.Sprint(stats.LastSignedBlockHeight), formattedTime(stats.LastSignedBlockTimestamp))
			}
			description = fmt.Sprintf("%s\n%s\n%s%s",
				latestBlock, lastSignedBlock, recentSignedBlocks, sentryString)
		}
	}

	color := getColorForAlertLevel(stats.AlertLevel)

	return discord.Embed{
		Title:       title,
		Description: description,
		Color:       color,
	}
}

func (service *DiscordNotificationService) client() (*webhook.Client, error) {
	if service.webhookURL != "" {
		fmt.Printf("[Discord] Creating webhook client using full URL\n")
		
		// Extract base URL and webhook credentials from the full webhook URL
		u, err := url.Parse(service.webhookURL)
		if err != nil {
			fmt.Printf("[Discord] Error: Failed to parse webhook URL: %v\n", err)
			return nil, err
		}
		
		// Extract ID and Token from path
		parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
		if len(parts) < 4 {
			fmt.Printf("[Discord] Error: Invalid webhook URL format\n")
			return nil, fmt.Errorf("invalid webhook URL format")
		}
		
		token := parts[3]
		id, err := snowflake.Parse(parts[2])
		if err != nil {
			fmt.Printf("[Discord] Error: Failed to parse webhook ID: %v\n", err)
			return nil, err
		}
		
		// Construct base URL (e.g., https://discord.com/api/v10)
		baseURL := fmt.Sprintf("%s://%s/%s", u.Scheme, u.Host, strings.Join(parts[:len(parts)-2], "/"))
		fmt.Printf("[Discord] Detected base URL: %s\n", baseURL)
		
		// Create webhook client with configured base URL
		client := webhook.New(id, token,
			webhook.WithRestClientConfigOpts(
				rest.WithURL(baseURL),
			),
		)
		fmt.Printf("[Discord] Successfully created webhook client from URL\n")
		return client, nil
	}
	fmt.Printf("[Discord] Creating webhook client using ID+Token (ID: %s)\n", service.webhookID)
	id := snowflake.MustParse(service.webhookID)
	client := webhook.New(id, service.webhookToken)
	fmt.Printf("[Discord] Successfully created webhook client from ID+Token\n")
	return client, nil
}

// implements NotificationService interface
func (service *DiscordNotificationService) UpdateValidatorRealtimeStatus(
	configFile string,
	config *HalfLifeConfig,
	vm *ValidatorMonitor,
	stats ValidatorStats,
	writeConfigMutex *sync.Mutex,
) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(time.Second*4))
	defer cancel()
	client, err := service.client()
	if err != nil {
		fmt.Printf("[Discord] Error creating discord webhook client: %v\n", err)
		return
	}
	defer client.Close(ctx)
	if vm.DiscordStatusMessageID != nil {
		service.postMutex.Lock()
		messageID := snowflake.MustParse(*vm.DiscordStatusMessageID)
		fmt.Printf("[Discord] Updating existing status message (ID: %s) for validator: %s\n", *vm.DiscordStatusMessageID, vm.Name)
		_, err := client.UpdateMessage(messageID, discord.WebhookMessageUpdate{
			Embeds: &[]discord.Embed{
				getCurrentStatsEmbed(stats, vm),
			},
		}, rest.UpdateWebhookMessageParams{})
		service.postMutex.Unlock()
		if err != nil {
			fmt.Printf("[Discord] Error updating discord message: %v\n", err)
			return
		}
		fmt.Printf("[Discord] Successfully updated status message for validator: %s\n", vm.Name)
	} else {
		service.postMutex.Lock()
		fmt.Printf("[Discord] Creating new status message for validator: %s\n", vm.Name)
		message, err := client.CreateMessage(discord.WebhookMessageCreate{
			Username: config.Notifications.Discord.Username,
			Embeds: []discord.Embed{
				getCurrentStatsEmbed(stats, vm),
			},
		}, rest.CreateWebhookMessageParams{Wait: true})
		service.postMutex.Unlock()
		if err != nil {
			fmt.Printf("[Discord] Error sending discord message: %v\n", err)
			return
		}
		messageID := string(message.ID)
		vm.DiscordStatusMessageID = &messageID
		fmt.Printf("[Discord] Created new status message (ID: %s) for validator: %s\n", messageID, vm.Name)
		saveConfig(configFile, config, writeConfigMutex)
	}
}

// implements NotificationService interface
func (service *DiscordNotificationService) SendValidatorAlertNotification(
	config *HalfLifeConfig,
	vm *ValidatorMonitor,
	stats ValidatorStats,
	alertNotification *ValidatorAlertNotification,
) {
	tagUser := ""
	for _, userID := range config.Notifications.Discord.AlertUserIDs {
		tagUser += fmt.Sprintf("<@%s> ", userID)
	}

	var embedTitle string
	if vm.FullNode {
		embedTitle = vm.Name
	} else {
		if stats.SlashingPeriodUptime > 0 {
			embedTitle = fmt.Sprintf("%s (%.02f%% up)", vm.Name, stats.SlashingPeriodUptime)
		} else {
			embedTitle = fmt.Sprintf("%s (N/A%% up)", vm.Name)
		}
	}

	if len(alertNotification.Alerts) > 0 {
		alertString := ""
		for _, alert := range alertNotification.Alerts {
			alertString += fmt.Sprintf("\n• %s", alert)
		}
		alertColor := getColorForAlertLevel(alertNotification.AlertLevel)
		toNotify := ""
		if alertNotification.AlertLevel > alertLevelWarning {
			toNotify = strings.Trim(tagUser, " ")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(time.Second*4))
		defer cancel()
		fmt.Printf("[Discord] Sending alert notification for validator: %s (AlertLevel: %d)\n", vm.Name, alertNotification.AlertLevel)
		client, err := service.client()
		if err != nil {
			fmt.Printf("[Discord] Error creating discord webhook client: %v\n", err)
			ctx.Done()
			return
		}
		defer client.Close(ctx)
		service.postMutex.Lock()
		_, err = client.CreateMessage(discord.WebhookMessageCreate{
			Username: config.Notifications.Discord.Username,
			Content:  toNotify,
			Embeds: []discord.Embed{
				discord.Embed{
					Title:       embedTitle,
					Description: fmt.Sprintf("**Errors:**\n%s", strings.Trim(alertString, "\n")),
					Color:       alertColor,
				},
			},
		}, rest.CreateWebhookMessageParams{Wait: true})
		service.postMutex.Unlock()
		if err != nil {
			fmt.Printf("[Discord] Error sending alert notification: %v\n", err)
		} else {
			fmt.Printf("[Discord] Successfully sent alert notification for validator: %s\n", vm.Name)
		}
	}

	if len(alertNotification.ClearedAlerts) > 0 {
		clearedAlertsString := ""
		for _, alert := range alertNotification.ClearedAlerts {
			clearedAlertsString += fmt.Sprintf("\n• %s", alert)
		}
		toNotify := ""
		if alertNotification.NotifyForClear {
			toNotify = strings.Trim(tagUser, " ")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(time.Second*4))
		defer cancel()
		fmt.Printf("[Discord] Sending cleared alerts notification for validator: %s\n", vm.Name)
		client, err := service.client()
		if err != nil {
			fmt.Printf("[Discord] Error creating discord webhook client: %v\n", err)
			ctx.Done()
			return
		}
		defer client.Close(ctx)
		service.postMutex.Lock()
		_, err = client.CreateMessage(discord.WebhookMessageCreate{
			Username: config.Notifications.Discord.Username,
			Content:  toNotify,
			Embeds: []discord.Embed{
				discord.Embed{
					Title:       embedTitle,
					Description: fmt.Sprintf("**Errors cleared:**\n%s", strings.Trim(clearedAlertsString, "\n")),
					Color:       colorGood,
				},
			},
		}, rest.CreateWebhookMessageParams{Wait: true})
		service.postMutex.Unlock()
		if err != nil {
			fmt.Printf("[Discord] Error sending cleared alerts notification: %v\n", err)
		} else {
			fmt.Printf("[Discord] Successfully sent cleared alerts notification for validator: %s\n", vm.Name)
		}
	}
}
