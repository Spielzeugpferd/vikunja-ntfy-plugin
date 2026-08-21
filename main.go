// Yaegi evaluates this at runtime via its exported factories, so there is no func main
// and it must stay out of `go build ./...`.
//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"code.vikunja.io/api/pkg/config"
	"code.vikunja.io/api/pkg/db"
	"code.vikunja.io/api/pkg/events"
	"code.vikunja.io/api/pkg/log"
	"code.vikunja.io/api/pkg/models"
	"code.vikunja.io/api/pkg/plugins"
	"code.vikunja.io/api/pkg/user"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/labstack/echo/v5"
	"src.techknowlogick.com/xormigrate"
	"xorm.io/xorm"
)

const targetTable = "plugin_ntfy_targets"

// NtfyPlugin forwards Vikunja notifications directly to one or more ntfy
// (https://github.com/binwiederhier/ntfy) topics — no third-party bridge in
// between. Unlike the sibling vikunja-apprise plugin, ntfy has no built-in
// per-key persistent config store, so this plugin keeps a small local table of
// each user's own topics (and, if their topic is protected, an access token).
type NtfyPlugin struct{}

func (p *NtfyPlugin) Name() string    { return "ntfy-notifications" }
func (p *NtfyPlugin) Version() string { return "0.1.0" }

func (p *NtfyPlugin) Init() error {
	events.RegisterListener((&models.TaskReminderFiredEvent{}).Name(), &ReminderListener{})
	events.RegisterListener((&models.TaskOverdueEvent{}).Name(), &OverdueListener{})
	events.RegisterListener((&models.TasksOverdueEvent{}).Name(), &OverdueDigestListener{})
	// pkg/notifications isn't exposed to yaegi, so the event is registered by its
	// well-known string name instead of notifications.NotificationCreatedEvent{}.Name().
	events.RegisterListener("notification.created", &GenericNotificationListener{})

	log.Infof("ntfy-notifications plugin initialized")
	return nil
}

func (p *NtfyPlugin) Shutdown() error { return nil }

// RegisterAuthenticatedRoutes implements the AuthenticatedRouterPlugin interface
func (p *NtfyPlugin) RegisterAuthenticatedRoutes(g *echo.Group) {
	g.POST("/ntfy/config", handleSetConfig)
	g.GET("/ntfy/config", handleGetConfig)
	g.DELETE("/ntfy/config", handleDeleteConfig)

	log.Infof("ntfy-notifications plugin routes registered")
}

// Migrations implements the MigrationPlugin interface
func (p *NtfyPlugin) Migrations() []*xormigrate.Migration {
	return []*xormigrate.Migration{
		{
			ID:          "20260821130000-create-plugin-ntfy-targets",
			Description: "Create table for per-user ntfy publish targets",
			Migrate: func(tx *xorm.Engine) error {
				return tx.Table(targetTable).Sync2(&ntfyTarget{})
			},
			Rollback: func(tx *xorm.Engine) error {
				return tx.DropTables(targetTable)
			},
		},
	}
}

// Interpreted types reach xorm as anonymous reflect structs with no methods, so
// TableName() is invisible — every query below passes targetTable to Table() explicitly.
type ntfyTarget struct {
	ID      int64     `xorm:"pk autoincr"`
	UserID  int64     `xorm:"bigint index not null"`
	Server  string    `xorm:"varchar(255) not null"`
	Topic   string    `xorm:"varchar(255) not null"`
	Token   string    `xorm:"varchar(255) not null"`
	Created time.Time `xorm:"created"`
}

// os.Getenv does not see the host process's real environment from inside a
// Yaegi-interpreted plugin (confirmed by live testing: it always returns "",
// even though the same env var is visibly set on the running process via ps).
// config.Key wraps viper directly — viper.AutomaticEnv() with SetEnvPrefix("vikunja")
// still binds VIKUNJA_PLUGINS_NTFY_DEFAULTSERVER to this key without Vikunja core
// having to pre-declare it, and the actual env lookup happens in native code, not
// through the interpreter, so it isn't affected by whatever breaks os.Getenv here.
func defaultServer() string {
	if v := config.Key("plugins.ntfy.defaultserver").GetString(); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://ntfy.sh"
}

// --- ntfy client -------------------------------------------------------------
//
// A protected ntfy topic's access token lives in this table because ntfy itself
// has no config-store equivalent to Apprise API's /add/<key> to offload it to.
// Anyone who can read this plugin's config routes for a given user can read
// that user's own token back — same as ntfy's own behavior for its own topics,
// but worth keeping in mind if this table is ever exposed more broadly.

func sendNtfy(target *ntfyTarget, title, body string) error {
	payload, err := json.Marshal(map[string]string{
		"topic":   target.Topic,
		"title":   title,
		"message": body,
	})
	if err != nil {
		return err
	}

	server := target.Server
	if server == "" {
		server = defaultServer()
	}

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(server, "/")+"/", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if target.Token != "" {
		req.Header.Set("Authorization", "Bearer "+target.Token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy server responded with %s", resp.Status)
	}
	return nil
}

func notifyUser(userID int64, title, body string) {
	s := db.NewSession()
	defer s.Close()

	targets := []*ntfyTarget{}
	if err := s.Table(targetTable).Where("user_id = ?", userID).Find(&targets); err != nil {
		log.Errorf("ntfy-notifications: could not load targets for user %d: %s", userID, err)
		return
	}

	for _, t := range targets {
		if err := sendNtfy(t, title, body); err != nil {
			log.Errorf("ntfy-notifications: push failed for user %d topic %s: %s", userID, t.Topic, err)
		}
	}
}

// --- Config routes -----------------------------------------------------------

type targetRequest struct {
	Server string `json:"server"`
	Topic  string `json:"topic"`
	Token  string `json:"token"`
}

type configRequest struct {
	Targets []targetRequest `json:"targets"`
}

func handleSetConfig(c *echo.Context) error {
	s := db.NewSession()
	defer s.Close()

	u, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "user not found")
	}

	req := &configRequest{}
	if err := c.Bind(req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	if len(req.Targets) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "targets must contain at least one topic")
	}
	for _, t := range req.Targets {
		if t.Topic == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "every target needs a topic")
		}
	}

	// Full replace, mirroring how Apprise API's /add/<key> replaces a user's whole set.
	if _, err := s.Table(targetTable).Where("user_id = ?", u.ID).Delete(&ntfyTarget{}); err != nil {
		_ = s.Rollback()
		return err
	}

	for _, t := range req.Targets {
		row := &ntfyTarget{
			UserID: u.ID,
			Server: strings.TrimRight(t.Server, "/"),
			Topic:  t.Topic,
			Token:  t.Token,
		}
		if _, err := s.Table(targetTable).Insert(row); err != nil {
			_ = s.Rollback()
			return err
		}
	}

	// db.NewSession() opens a transaction; without this, s.Close() (deferred above)
	// rolls everything in this handler back instead of persisting it.
	if err := s.Commit(); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func handleGetConfig(c *echo.Context) error {
	s := db.NewSession()
	defer s.Close()

	u, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "user not found")
	}

	targets := []*ntfyTarget{}
	if err := s.Table(targetTable).Where("user_id = ?", u.ID).Find(&targets); err != nil {
		return err
	}

	return c.JSON(http.StatusOK, map[string]interface{}{"targets": targets})
}

func handleDeleteConfig(c *echo.Context) error {
	s := db.NewSession()
	defer s.Close()

	u, err := user.GetCurrentUserFromDB(s, c)
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "user not found")
	}

	if _, err := s.Table(targetTable).Where("user_id = ?", u.ID).Delete(&ntfyTarget{}); err != nil {
		_ = s.Rollback()
		return err
	}
	if err := s.Commit(); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// --- task.reminder.fired -----------------------------------------------------
// Dispatched unconditionally (gated only by the instance-wide webhooks.enabled
// setting, not by the user's own email-reminder preference), unlike the DB-backed
// ReminderDueNotification, which task_reminder.go only persists when the user has
// email reminders enabled. Listening here means push reminders work independently
// of that email setting.

type reminderFiredPayload struct {
	Task struct {
		Title string `json:"title"`
	} `json:"task"`
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
	Project struct {
		Title string `json:"title"`
	} `json:"project"`
}

type ReminderListener struct{}

func (l *ReminderListener) Name() string { return "ntfy.taskReminderFired" }

func (l *ReminderListener) Handle(msg *message.Message) error {
	p := &reminderFiredPayload{}
	if err := json.Unmarshal(msg.Payload, p); err != nil {
		return err
	}
	notifyUser(p.User.ID, "Reminder: "+p.Task.Title, p.Project.Title)
	return nil
}

// --- task.overdue / tasks.overdue --------------------------------------------
// UndoneTaskOverdueNotification/UndoneTasksOverdueNotification have ToDB() return
// nil, so they never reach the notifications table or fire notification.created.
// These dedicated events are the only hook for overdue pushes.

type overdueSinglePayload struct {
	Task struct {
		Title string `json:"title"`
	} `json:"task"`
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
	Project struct {
		Title string `json:"title"`
	} `json:"project"`
}

type OverdueListener struct{}

func (l *OverdueListener) Name() string { return "ntfy.taskOverdue" }

func (l *OverdueListener) Handle(msg *message.Message) error {
	p := &overdueSinglePayload{}
	if err := json.Unmarshal(msg.Payload, p); err != nil {
		return err
	}
	notifyUser(p.User.ID, "Overdue: "+p.Task.Title, p.Project.Title)
	return nil
}

type overdueMultiPayload struct {
	Tasks []struct {
		Title string `json:"title"`
	} `json:"tasks"`
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

type OverdueDigestListener struct{}

func (l *OverdueDigestListener) Name() string { return "ntfy.tasksOverdue" }

func (l *OverdueDigestListener) Handle(msg *message.Message) error {
	p := &overdueMultiPayload{}
	if err := json.Unmarshal(msg.Payload, p); err != nil {
		return err
	}

	titles := make([]string, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		titles = append(titles, t.Title)
	}

	title := fmt.Sprintf("%d tasks overdue", len(titles))
	notifyUser(p.User.ID, title, strings.Join(titles, ", "))
	return nil
}

// --- notification.created (catch-all for everything else) -------------------

// dbNotification mirrors the columns of Vikunja's own notifications table that
// this plugin needs. Interpreted types reach xorm as anonymous reflect structs
// with no methods, so every query passes the table name explicitly via Table().
type dbNotification struct {
	ID           int64           `xorm:"pk autoincr"`
	NotifiableID int64           `xorm:"bigint not null"`
	Notification json.RawMessage `xorm:"json not null"`
	Name         string          `xorm:"varchar(250) not null"`
}

// genericNotificationPayload declares every optional field any notification's
// ToDB() JSON might contain; fields absent from a given payload just stay zero.
type genericNotificationPayload struct {
	Task struct {
		Title string `json:"title"`
	} `json:"task"`
	Project struct {
		Title string `json:"title"`
	} `json:"project"`
	Doer struct {
		Username string `json:"username"`
	} `json:"doer"`
	Assignee struct {
		Username string `json:"username"`
	} `json:"assignee"`
	Comment struct {
		Comment string `json:"comment"`
	} `json:"comment"`
	Member struct {
		Username string `json:"username"`
	} `json:"member"`
	Team struct {
		Name string `json:"name"`
	} `json:"team"`
}

func describeNotification(name string, raw json.RawMessage) (title, body string) {
	p := &genericNotificationPayload{}
	if err := json.Unmarshal(raw, p); err != nil {
		return "", ""
	}

	switch name {
	case "task.comment":
		return "New comment on " + p.Task.Title, p.Doer.Username + ": " + p.Comment.Comment
	case "task.assigned":
		return "Task assigned: " + p.Task.Title, "Assigned to " + p.Assignee.Username + " by " + p.Doer.Username
	case "task.deleted":
		return "Task deleted: " + p.Task.Title, "Deleted by " + p.Doer.Username
	case "project.created":
		return "New project: " + p.Project.Title, "Created by " + p.Doer.Username
	case "team.member.added":
		return "Added to team " + p.Team.Name, "By " + p.Doer.Username
	case "task.mentioned":
		return "Mentioned in " + p.Task.Title, "By " + p.Doer.Username
	default:
		return "Vikunja notification", name
	}
}

type GenericNotificationListener struct{}

func (l *GenericNotificationListener) Name() string { return "ntfy.notificationCreated" }

func (l *GenericNotificationListener) Handle(msg *message.Message) error {
	created := &struct {
		NotificationID int64 `json:"notification_id"`
		UserID         int64 `json:"user_id"`
	}{}
	if err := json.Unmarshal(msg.Payload, created); err != nil {
		return err
	}

	s := db.NewSession()
	defer s.Close()

	row := &dbNotification{}
	has, err := s.Table("notifications").Where("id = ?", created.NotificationID).Get(row)
	if err != nil {
		return err
	}
	// task.reminder is already handled by ReminderListener via task.reminder.fired;
	// forwarding it again here would double-send.
	if !has || row.Name == "task.reminder" {
		return nil
	}

	title, body := describeNotification(row.Name, row.Notification)
	if title == "" {
		return nil
	}

	notifyUser(created.UserID, title, body)
	return nil
}

var singleton = &NtfyPlugin{}

func NewPlugin() plugins.Plugin { return singleton }

// Typed factory functions for Yaegi compatibility.
// Yaegi wraps return values per the declared return type, so sub-interface type
// assertions (Plugin -> AuthenticatedRouterPlugin) don't work. These typed
// factories ensure Yaegi wraps the value with the correct interface wrapper.
func NewAuthenticatedRouterPlugin() plugins.AuthenticatedRouterPlugin { return singleton }
func NewMigrationPlugin() plugins.MigrationPlugin                     { return singleton }
