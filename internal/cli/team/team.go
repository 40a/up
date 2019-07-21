package team

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/segmentio/go-snakecase"
	"github.com/stripe/stripe-go"
	"github.com/stripe/stripe-go/token"
	"github.com/tj/go/clipboard"
	"github.com/tj/go/env"
	"github.com/tj/go/http/request"
	"github.com/tj/go/term"
	"github.com/tj/kingpin"
	"github.com/tj/survey"

	"github.com/apex/log"
	"github.com/apex/up/internal/account"
	"github.com/apex/up/internal/cli/root"
	"github.com/apex/up/internal/colors"
	"github.com/apex/up/internal/stats"
	"github.com/apex/up/internal/userconfig"
	"github.com/apex/up/internal/util"
	"github.com/apex/up/platform/event"
	"github.com/apex/up/reporter"
)

// api endpoint.
var api = env.GetDefault("APEX_TEAMS_API", "https://teams.apex.sh")

// api client.
var a = account.New(api)

// plan amounts.
var amounts = map[string]int{
	"monthly":  2000,
	"annually": 21600,
}

// plan amount select options.
var amountOptions = map[string]string{
	"Monthly at $20.00 USD":   "monthly",
	"Annually at $216.00 USD": "annually",
}

func init() {
	cmd := root.Command("team", "Manage team members, plans, and billing.")
	cmd.Example(`up team`, "Show active team and subscription status.")
	cmd.Example(`up team login`, "Sign in or create account with interactive prompt.")
	cmd.Example(`up team login --email tj@example.com --team apex-software`, "Sign in to a team.")
	cmd.Example(`up team add "Apex Software"`, "Add a new team.")
	cmd.Example(`up team subscribe`, "Subscribe to the Pro plan.")
	cmd.Example(`up team members add asya@example.com`, "Invite a team member to your active team.")
	cmd.Example(`up team members rm tobi@example.com`, "Remove a team member from your active team.")
	cmd.Example(`up team card change`, "Change the default credit card.")
	cmd.Example(`up team switch`, "Switch teams interactively.")
	status(cmd)
	switchTeam(cmd)
	login(cmd)
	logout(cmd)
	members(cmd)
	subscribe(cmd)
	unsubscribe(cmd)
	card(cmd)
	copy(cmd)
	add(cmd)
}

// add command.
func add(cmd *kingpin.Cmd) {
	c := cmd.Command("add", "Add a new team.")
	name := c.Arg("name", "Name of the team.").Required().String()

	c.Action(func(_ *kingpin.ParseContext) error {
		var config userconfig.Config
		if err := config.Load(); err != nil {
			return errors.Wrap(err, "loading config")
		}

		if !config.Authenticated() {
			return errors.New("Must sign in to create a new team.")
		}

		team := strings.Replace(snakecase.Snakecase(*name), "_", "-", -1)

		stats.Track("Add Team", map[string]interface{}{
			"team": team,
			"name": name,
		})

		t := config.GetActiveTeam()

		if err := a.AddTeam(t.Token, team, *name); err != nil {
			return errors.Wrap(err, "creating team")
		}

		defer util.Pad()()
		util.Log("Created team %s with id %s", *name, team)

		code, err := a.LoginWithToken(t.Token, t.Email, team)
		if err != nil {
			return errors.Wrap(err, "login")
		}

		// access key
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		token, err := a.PollAccessToken(ctx, t.Email, team, code)
		if err != nil {
			return errors.Wrap(err, "getting access token")
		}

		err = userconfig.Alter(func(c *userconfig.Config) {
			c.Team = team
			c.AddTeam(&userconfig.Team{
				Token: token,
				ID:    team,
				Email: t.Email,
			})
		})

		if err != nil {
			return errors.Wrap(err, "config")
		}

		util.Log("%s is now the active team.\n", *name)
		util.Log("Use `up team switch` to change teams.")

		return nil
	})
}

// copy commands.
func copy(cmd *kingpin.Cmd) {
	c := cmd.Command("ci", "Credentials for CI.")
	copy := c.Flag("copy", "Credentials to the clipboard.").Short('c').Bool()

	c.Action(func(_ *kingpin.ParseContext) error {
		var config userconfig.Config
		if err := config.Load(); err != nil {
			return errors.Wrap(err, "loading")
		}

		stats.Track("Copy Credentials", map[string]interface{}{
			"copy": *copy,
		})

		b, err := json.Marshal(config)
		if err != nil {
			return errors.Wrap(err, "marshaling")
		}

		s := base64.StdEncoding.EncodeToString(b)

		if *copy {
			clipboard.Write(s)
			fmt.Println("Copied to clipboard!")
			return nil
		}

		fmt.Printf("%s\n", s)

		return nil
	})
}

// status of account.
func status(cmd *kingpin.Cmd) {
	c := cmd.Command("status", "Status of your account.").Default()

	c.Action(func(_ *kingpin.ParseContext) error {
		var config userconfig.Config
		if err := config.Load(); err != nil {
			return errors.Wrap(err, "loading config")
		}

		defer util.Pad()()
		stats.Track("Account Status", nil)

		if !config.Authenticated() {
			util.LogName("status", "Signed out")
			return nil
		}

		t := config.GetActiveTeam()

		defer util.Pad()()
		util.LogName("team", t.ID)

		team, err := a.GetTeam(t.Token)
		if err != nil {
			return errors.Wrap(err, "fetching team")
		}

		plans, err := a.GetPlans(t.Token)
		if err != nil {
			return errors.Wrap(err, "fetching plans")
		}

		if len(plans) == 0 {
			util.LogName("subscription", "none")
			return nil
		}

		if c := team.Card; c != nil {
			util.LogName("card", "%s ending with %s", c.Brand, c.LastFour)
		}

		// TODO: filter on plan type (later may be other products)
		p := plans[0]

		if d := p.Discount; d != nil {
			p.Amount = d.Coupon.Discount(p.Amount)
			util.LogName("coupon", d.Coupon.ID)
		}

		util.LogName("amount", "%s USD per %s", currency(p.Amount), p.Interval)
		util.LogName("owner", team.Owner)
		util.LogName("created", p.CreatedAt.Format("January 2, 2006"))
		if p.Canceled {
			util.LogName("canceled", p.CanceledAt.Format("January 2, 2006"))
		}

		return nil
	})
}

// switchTeam team.
func switchTeam(cmd *kingpin.Cmd) {
	c := cmd.Command("switch", "Switch active team.")
	c.Example(`up team switch`, "Switch teams interactively.")

	c.Action(func(_ *kingpin.ParseContext) error {
		defer util.Pad()()

		var config userconfig.Config
		if err := config.Load(); err != nil {
			return errors.Wrap(err, "loading user config")
		}

		var options []string
		for _, t := range config.GetTeams() {
			options = append(options, t.ID)
		}
		sort.Strings(options)

		var team string
		prompt := &survey.Select{
			Message: "",
			Options: options,
			Default: config.Team,
		}

		if err := survey.AskOne(prompt, &team, survey.Required); err != nil {
			return err
		}

		stats.Track("Switch Team", nil)

		err := userconfig.Alter(func(c *userconfig.Config) {
			c.Team = team
		})

		if err != nil {
			return errors.Wrap(err, "saving config")
		}

		return nil
	})
}

// login user.
func login(cmd *kingpin.Cmd) {
	c := cmd.Command("login", "Sign in to your account.")
	c.Example(`up team login`, "Sign in or create account with interactive prompt.")
	c.Example(`up team login --team apex-software`, "Sign in to a team using your existing email.")
	c.Example(`up team login --email tj@example.com --team apex-software`, "Sign in to a team with email.")
	email := c.Flag("email", "Email address.").String()
	team := c.Flag("team", "Team id.").String()

	c.Action(func(_ *kingpin.ParseContext) error {
		var config userconfig.Config
		if err := config.Load(); err != nil {
			return errors.Wrap(err, "loading user config")
		}

		defer util.Pad()()

		stats.Track("Login", map[string]interface{}{
			"team_count": len(config.GetTeams()),
			"has_email":  *email != "",
			"has_team":   *team != "",
		})

		t := config.GetActiveTeam()

		// both team and email are specified,
		// so we want to disregard the active team
		// entirely and sign in using these creds.
		if *email != "" && *team != "" {
			t = nil
		}

		// ensure we have an email
		if *email == "" {
			if t == nil {
				if err := prompt("email:", email); err != nil {
					return err
				}
			} else {
				*email = t.Email
			}
		}

		// ensure we have a team if already signed-in,
		// this lets the user specify only --team xxx to
		// join a team they were invited to, or add one
		// which they own.
		if t != nil && *team == "" {
			util.Log("Already signed in as %s on team %s.", t.Email, t.ID)
			util.Log("Use `up team login --team <id>` to join a team.")
			return nil
		}

		// events
		events := make(event.Events)
		go reporter.Text(events)
		events.Emit("account.login.verify", nil)

		l := log.WithFields(log.Fields{
			"email": *email,
			"team":  *team,
		})

		// authenticate
		var code string
		var err error
		if t == nil {
			l.Debug("login without token")
			code, err = a.Login(*email, *team)
		} else {
			l.Debug("login with token")
			code, err = a.LoginWithToken(t.Token, *email, *team)
		}

		if err != nil {
			return errors.Wrap(err, "login")
		}

		// personal team
		if *team == "" {
			team = email
		}

		// access key
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		l.WithField("team", *team).Debug("poll for access token")
		token, err := a.PollAccessToken(ctx, *email, *team, code)
		if err != nil {
			return errors.Wrap(err, "getting access token")
		}

		events.Emit("account.login.verified", nil)
		err = userconfig.Alter(func(c *userconfig.Config) {
			c.Team = *team
			c.AddTeam(&userconfig.Team{
				Token: token,
				ID:    *team,
				Email: *email,
			})
		})

		if err != nil {
			return errors.Wrap(err, "config")
		}

		return nil
	})
}

// logout user.
func logout(cmd *kingpin.Cmd) {
	c := cmd.Command("logout", "Sign out of your account.")

	c.Action(func(_ *kingpin.ParseContext) error {
		stats.Track("Logout", nil)

		var config userconfig.Config
		if err := config.Save(); err != nil {
			return errors.Wrap(err, "saving")
		}

		util.LogPad("Signed out")

		return nil
	})
}

// subscribe to plan.
func subscribe(cmd *kingpin.Cmd) {
	c := cmd.Command("subscribe", "Subscribe to the Pro plan.")

	c.Action(func(_ *kingpin.ParseContext) error {
		t, err := userconfig.Require()
		if err != nil {
			return err
		}

		defer util.Pad()()

		// plan
		util.LogTitle("Subscription")
		util.Log("Choose a monthly billing period, or 10%% off annually.")
		println()

		var interval string
		err = survey.AskOne(&survey.Select{
			Message: "Plan:",
			Options: keys(amountOptions),
		}, &interval, survey.Required)

		// amount
		interval = amountOptions[interval]
		amount := amounts[interval]

		util.LogTitle("Coupon")
		util.Log("Enter a coupon, or press enter to skip this step")
		util.Log("and move on to adding a credit card.")
		println()

		// coupon
		var couponID string
		err = survey.AskOne(&survey.Input{
			Message: "Coupon:",
		}, &couponID, nil)

		if err != nil {
			return err
		}

		// coupon
		if strings.TrimSpace(couponID) == "" {
			util.LogClear("No coupon provided")
		} else {
			coupon, err := a.GetCoupon(couponID)
			if err != nil && !request.IsNotFound(err) {
				return errors.Wrap(err, "fetching coupon")
			}

			if coupon == nil {
				util.LogClear("Coupon is invalid")
			} else {
				amount = coupon.Discount(amount)
				msg := colors.Gray(fmt.Sprintf("%s — now %s %s", coupon.Description(), currency(amount), interval))
				util.LogClear("Savings: %s", msg)
			}
		}

		// add card
		util.LogTitle("Credit Card")
		util.Log("First add your credit card details, these are transferred")
		util.Log("directly to Stripe over HTTPS and never touch our servers.")
		println()

		card, err := account.PromptForCard()
		if err != nil {
			return errors.Wrap(err, "prompting for card")
		}

		tok, err := token.New(&stripe.TokenParams{
			Card: &card,
		})

		if err != nil {
			return errors.Wrap(err, "requesting card token")
		}

		if err := a.AddCard(t.Token, tok.ID); err != nil {
			return errors.Wrap(err, "adding card")
		}

		util.LogTitle("Confirm")

		// confirm
		var ok bool
		err = survey.AskOne(&survey.Confirm{
			Message: fmt.Sprintf("Subscribe to Up Pro for %s USD %s?", currency(amount), interval),
		}, &ok, nil)

		if err != nil {
			return err
		}

		if !ok {
			util.LogPad("Aborted")
			stats.Track("Abort Subscription", nil)
			return nil
		}

		stats.Track("Subscribe", map[string]interface{}{
			"coupon":   couponID,
			"interval": interval,
		})

		if err := a.AddPlan(t.Token, "up", interval, couponID); err != nil {
			return errors.Wrap(err, "subscribing")
		}

		util.LogClear("Thanks for subscribing! Run `up upgrade` to install the Pro release.")

		return nil
	})
}

// unsubscribe from plan.
func unsubscribe(cmd *kingpin.Cmd) {
	c := cmd.Command("unsubscribe", "Unsubscribe from the Pro plan.")

	c.Action(func(_ *kingpin.ParseContext) error {
		config, err := userconfig.Require()
		if err != nil {
			return err
		}

		defer util.Pad()()

		// confirm
		var ok bool
		err = survey.AskOne(&survey.Confirm{
			Message: "Are you sure you want to unsubscribe?",
		}, &ok, nil)

		if err != nil {
			return err
		}

		if !ok {
			util.LogPad("Aborted")
			return nil
		}

		term.MoveUp(1)
		term.ClearLine()

		msg, err := feedback()
		if err != nil {
			util.LogPad("Aborted")
			return nil
		}

		if strings.TrimSpace(msg) != "" {
			util.Log("Thanks for the feedback!")
			_ = a.AddFeedback(config.Token, msg)
		}

		stats.Track("Unsubscribe", nil)

		if err := a.RemovePlan(config.Token, "up"); err != nil {
			return errors.Wrap(err, "unsubscribing")
		}

		util.LogClear("Unsubscribed!")

		return nil
	})
}

// members commands.
func members(cmd *kingpin.Cmd) {
	c := cmd.Command("members", "Member management.")
	addMember(c)
	removeMember(c)
	listMembers(c)
}

// addMember command.
func addMember(cmd *kingpin.Cmd) {
	c := cmd.Command("add", "Add invites a team member.")
	c.Example(`up team members add asya@apex.sh`, "Invite a team member to the active team.")
	email := c.Arg("email", "Email address.").Required().String()

	c.Action(func(_ *kingpin.ParseContext) error {
		t, err := userconfig.Require()
		if err != nil {
			return err
		}

		stats.Track("Add Member", map[string]interface{}{
			"team":  t.ID,
			"email": *email,
		})

		if err := a.AddInvite(t.Token, *email); err != nil {
			return errors.Wrap(err, "adding invite")
		}

		util.LogPad("Invited %s to team %s", *email, t.ID)

		return nil
	})
}

// removeMember command.
func removeMember(cmd *kingpin.Cmd) {
	c := cmd.Command("rm", "Remove a member or invite.").Alias("remove")
	c.Example(`up team members rm tobi@apex.sh`, "Remove a team member or invite from the active team.")
	email := c.Arg("email", "Email address.").Required().String()

	c.Action(func(_ *kingpin.ParseContext) error {
		t, err := userconfig.Require()
		if err != nil {
			return err
		}

		stats.Track("Remove Member", map[string]interface{}{
			"team":  t.ID,
			"email": *email,
		})

		if err := a.RemoveMember(t.Token, *email); err != nil {
			return errors.Wrap(err, "removing member")
		}

		util.LogPad("Removed %s from team %s", *email, t.ID)

		return nil
	})
}

// list members
func listMembers(cmd *kingpin.Cmd) {
	c := cmd.Command("ls", "List team members and invites.").Alias("list").Default()

	c.Action(func(_ *kingpin.ParseContext) error {
		t, err := userconfig.Require()
		if err != nil {
			return err
		}

		stats.Track("List Members", map[string]interface{}{
			"team": t.ID,
		})

		team, err := a.GetTeam(t.Token)
		if err != nil {
			return errors.Wrap(err, "fetching team")
		}

		defer util.Pad()()

		util.LogName("team", t.ID)

		if len(team.Members) > 0 {
			util.LogTitle("Members")
			for _, u := range team.Members {
				util.LogListItem(u.Email)
			}
		}

		if len(team.Invites) > 0 {
			util.LogTitle("Invites")
			for _, email := range team.Invites {
				util.LogListItem(email)
			}
		}

		return nil
	})
}

// card commands.
func card(cmd *kingpin.Cmd) {
	c := cmd.Command("card", "Card management.")
	changeCard(c)
}

// changeCard command.
func changeCard(cmd *kingpin.Cmd) {
	c := cmd.Command("change", "Change the default card.")

	c.Action(func(_ *kingpin.ParseContext) error {
		t, err := userconfig.Require()
		if err != nil {
			return err
		}

		defer util.Pad()()

		util.LogTitle("Credit Card")
		util.Log("Enter new credit card details to replace the existing card.")
		println()

		card, err := account.PromptForCard()
		if err != nil {
			return errors.Wrap(err, "prompting for card")
		}

		tok, err := token.New(&stripe.TokenParams{
			Card: &card,
		})

		if err != nil {
			return errors.Wrap(err, "requesting card token")
		}

		if err := a.AddCard(t.Token, tok.ID); err != nil {
			return errors.Wrap(err, "adding card")
		}

		println()
		util.Log("Updated")

		return nil
	})
}

// prompt helper.
func prompt(name string, s *string) error {
	prompt := &survey.Input{Message: name}
	return survey.AskOne(prompt, s, survey.Required)
}

// feedback prompt helper.
func feedback() (string, error) {
	var s string
	prompt := &survey.Input{Message: "Have any feedback? (optional)"}
	if err := survey.AskOne(prompt, &s, nil); err != nil {
		return "", err
	}
	return s, nil
}

// currency returns formatted currency.
func currency(n int) string {
	return fmt.Sprintf("$%0.2f", float64(n)/100)
}

// keys returns the keys of a string map.
func keys(m map[string]string) (v []string) {
	for k := range m {
		v = append(v, k)
	}

	sort.Slice(v, func(i int, j int) bool {
		return v[i] > v[j]
	})

	return
}
