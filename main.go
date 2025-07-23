package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

// GameSession represents a single session of playing a game
type GameSession struct {
	GameName  string    `json:"game_name"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Duration  float64   `json:"duration_seconds"` // Duration in seconds
}

// UserGameData stores all game sessions for a user
type UserGameData struct {
	Sessions []GameSession `json:"sessions"`
	// Map to track currently active game sessions for a user
	// Key: Game Name, Value: Start Time
	ActiveGames map[string]time.Time `json:"-"` // This field is not persisted
}

// DataStore holds all user game data
type DataStore struct {
	Users map[string]*UserGameData `json:"users"` // Key: User ID
	mu    sync.Mutex               // Mutex to protect concurrent access to Users map
}

const (
	dataFilePath = "game_data.json"
)

var (
	botToken string
	data     *DataStore
)

func init() {
	// Load Discord bot token from environment variable
	botToken = os.Getenv("DISCORD_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("DISCORD_BOT_TOKEN environment variable not set.")
	}

	// Initialize data store
	data = &DataStore{
		Users: make(map[string]*UserGameData),
	}

	// Load existing data from file
	if err := data.load(); err != nil {
		log.Printf("Could not load game data: %v. Starting with empty data.", err)
	}
}

func main() {
	// Create a new Discord session
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	// Register event handlers
	dg.AddHandler(ready)
	dg.AddHandler(presenceUpdate)
	dg.AddHandler(messageCreate)

	// We need to specify intents to receive presence updates and message content
	dg.Identify.Intents = discordgo.IntentsGuildPresences | discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

	// Open a websocket connection to Discord and begin listening
	err = dg.Open()
	if err != nil {
		log.Fatalf("Error opening connection: %v", err)
	}

	log.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc // Block until a signal is received

	// Cleanly close down the Discord session
	log.Println("Shutting down bot...")
	data.save() // Save data before closing
	dg.Close()
}

// ready function is called when the bot successfully connects to Discord
func ready(s *discordgo.Session, event *discordgo.Ready) {
	log.Printf("Logged in as: %v#%v", event.User.Username, event.User.Discriminator)
	s.UpdateGameStatus(0, "Tracking your games!")
}

// presenceUpdate is called when a user's presence (status, game activity) changes
func presenceUpdate(s *discordgo.Session, p *discordgo.PresenceUpdate) {
	// We only care about user presence updates, not bot presence updates
	if p.User.Bot {
		return
	}

	userID := p.User.ID
	username := p.User.Username

	data.mu.Lock()
	defer data.mu.Unlock()

	// Get or create user data
	userData, ok := data.Users[userID]
	if !ok {
		userData = &UserGameData{
			Sessions:    []GameSession{},
			ActiveGames: make(map[string]time.Time),
		}
		data.Users[userID] = userData
	}

	// Check current activities
	currentActivities := make(map[string]bool) // Map to quickly check active games from presence update
	for _, activity := range p.Activities {
		if activity.Type == discordgo.ActivityTypeGame {
			currentActivities[activity.Name] = true
		}
	}

	// Identify games that have stopped
	for gameName, startTime := range userData.ActiveGames {
		if !currentActivities[gameName] {
			// Game has stopped
			endTime := time.Now()
			duration := endTime.Sub(startTime).Seconds()
			session := GameSession{
				GameName:  gameName,
				StartTime: startTime,
				EndTime:   endTime,
				Duration:  duration,
			}
			userData.Sessions = append(userData.Sessions, session)
			delete(userData.ActiveGames, gameName) // Remove from active games
			log.Printf("User %s stopped playing %s. Duration: %.2f seconds", username, gameName, duration)
			data.save() // Save data after each session ends
		}
	}

	// Identify games that have started
	for _, activity := range p.Activities {
		if activity.Type == discordgo.ActivityTypeGame {
			gameName := activity.Name
			if _, isActive := userData.ActiveGames[gameName]; !isActive {
				// Game has started
				userData.ActiveGames[gameName] = time.Now()
				log.Printf("User %s started playing %s", username, gameName)
			}
		}
	}
}

// messageCreate is called when a new message is created in any channel the bot has access to
func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore messages from the bot itself
	if m.Author.ID == s.State.User.ID {
		return
	}

	// Check if the message is a command
	if m.Content == "!mygames" {
		userID := m.Author.ID
		username := m.Author.Username

		data.mu.Lock()
		defer data.mu.Unlock()

		userData, ok := data.Users[userID]
		if !ok || len(userData.Sessions) == 0 {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Hey %s, I haven't tracked any games for you yet!", username))
			return
		}

		// Calculate total play time per game
		gamePlayTimes := make(map[string]time.Duration)
		for _, session := range userData.Sessions {
			gamePlayTimes[session.GameName] += time.Duration(session.Duration) * time.Second
		}

		// Add currently active games to the total
		for gameName, startTime := range userData.ActiveGames {
			gamePlayTimes[gameName] += time.Since(startTime)
		}

		response := fmt.Sprintf("Here are your tracked game play times, %s:\n", username)
		for gameName, totalDuration := range gamePlayTimes {
			response += fmt.Sprintf("- **%s**: %s\n", gameName, formatDuration(totalDuration))
		}

		s.ChannelMessageSend(m.ChannelID, response)
	} else if m.Content == "!cleargames" {
		userID := m.Author.ID
		username := m.Author.Username

		data.mu.Lock()
		defer data.mu.Unlock()

		if _, ok := data.Users[userID]; ok {
			data.Users[userID] = &UserGameData{
				Sessions:    []GameSession{},
				ActiveGames: make(map[string]time.Time),
			}
			data.save()
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Hey %s, your game tracking data has been cleared!", username))
		} else {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Hey %s, you don't have any game data to clear!", username))
		}
	}
}

// formatDuration converts a time.Duration into a human-readable string
func formatDuration(d time.Duration) string {
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 { // Ensure at least seconds are shown for very short durations
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return fmt.Sprintf("%s", joinStrings(parts, " "))
}

func joinStrings(s []string, sep string) string {
	if len(s) == 0 {
		return ""
	}
	if len(s) == 1 {
		return s[0]
	}
	result := s[0]
	for i := 1; i < len(s); i++ {
		result += sep + s[i]
	}
	return result
}

// save persists the DataStore to a JSON file
func (ds *DataStore) save() error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	// Create a copy of the data to avoid issues with `ActiveGames` field during marshaling
	// as `ActiveGames` is marked with `json:"-"`
	tempUsers := make(map[string]*UserGameData)
	for userID, userData := range ds.Users {
		tempUsers[userID] = &UserGameData{
			Sessions: userData.Sessions,
			// ActiveGames is not saved, it's reconstructed on load or filled during runtime
		}
	}

	dataBytes, err := json.MarshalIndent(tempUsers, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling data: %w", err)
	}

	err = ioutil.WriteFile(dataFilePath, dataBytes, 0644)
	if err != nil {
		return fmt.Errorf("error writing data to file: %w", err)
	}
	log.Println("Game data saved successfully.")
	return nil
}

// load loads the DataStore from a JSON file
func (ds *DataStore) load() error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	dataBytes, err := ioutil.ReadFile(dataFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Data file %s does not exist. Starting with empty data.", dataFilePath)
			return nil // Not an error if file doesn't exist yet
		}
		return fmt.Errorf("error reading data file: %w", err)
	}

	tempUsers := make(map[string]*UserGameData)
	err = json.Unmarshal(dataBytes, &tempUsers)
	if err != nil {
		return fmt.Errorf("error unmarshaling data: %w", err)
	}

	// Re-initialize ActiveGames map for each user after loading
	for userID, userData := range tempUsers {
		userData.ActiveGames = make(map[string]time.Time)
		ds.Users[userID] = userData
	}

	log.Println("Game data loaded successfully.")
	return nil
}
