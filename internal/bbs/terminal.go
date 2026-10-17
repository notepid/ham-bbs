package bbs

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"hambbs/internal/callsign"
	"hambbs/internal/proto"
	"hambbs/internal/store"
	"hambbs/internal/transport"
)

// terminal serves a human using a plain packet terminal or telnet. Over a
// radio link the callsign comes from the link layer (as on every classic
// packet BBS); over TCP the user logs in with callsign and password.
type terminal struct {
	srv   *Server
	c     transport.Conn
	sess  *Session
	lr    *proto.LineReader
	w     *bufio.Writer
	nl    string
	call  string
	admin bool
}

const helpText = `Commands:
  L            list new (unread) messages
  LL [n]       list latest n messages (default 20)
  LM           list your personal mail
  LB [AREA]    list bulletins (optionally in one area)
  A            list bulletin areas
  R n          read message n
  S CALL       send personal mail to CALL
  SB AREA      send bulletin to AREA
  K n          delete message n (yours or addressed to you)
  I            info about this BBS
  B            bye (disconnect)`

func (t *terminal) printf(format string, args ...any) {
	fmt.Fprintf(t.w, format, args...)
}

// println writes text, converting embedded newlines to the link's ending.
func (t *terminal) println(text string) {
	t.w.WriteString(strings.ReplaceAll(text, "\n", t.nl))
	t.w.WriteString(t.nl)
}

func (t *terminal) readLine(timeout time.Duration) (string, error) {
	if err := t.w.Flush(); err != nil {
		return "", err
	}
	setDeadline(t.c, timeout)
	line, err := t.lr.ReadLine()
	return strings.TrimRight(line, " \t\x00"), err
}

func (t *terminal) run(first string) {
	if lc := t.c.LinkCall(); lc != "" {
		t.call = callsign.Base(lc)
		u, err := t.srv.store.GetUser(t.call)
		if errors.Is(err, store.ErrNotFound) {
			// Remember radio visitors so the sysop can see them; they can
			// claim the account later by syncing with a client.
			t.srv.store.CreateUser(t.call, nil, "", false)
		}
		t.admin = u.Admin
	} else {
		if !t.loginTCP(first) {
			t.w.Flush()
			return
		}
		first = ""
	}
	t.srv.setSessionCall(t.sess, t.call)
	t.srv.store.TouchUser(t.call)
	t.srv.event("%s connected (terminal via %s)", t.call, t.c.Kind())

	if first == "" {
		t.greet()
	}
	line := first
	for {
		if line != "" {
			if !t.command(line) {
				t.w.Flush()
				return
			}
		}
		t.printf("%s", t.srv.prompt())
		var err error
		if line, err = t.readLine(idleTimeout); err != nil {
			return
		}
	}
}

func (t *terminal) greet() {
	if motd := t.srv.cfg.MOTD; motd != "" {
		t.println(motd)
	}
	if n := t.unreadPersonal(); n > 0 {
		t.printf("You have %d new personal message(s). Type LM to list.%s", n, t.nl)
	}
	t.println("Type H for help.")
}

func (t *terminal) loginTCP(first string) bool {
	call := callsign.Normalize(first)
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			t.printf("Callsign: ")
			var err error
			if call, err = t.readLine(handshakeTimeout); err != nil {
				return false
			}
			call = callsign.Normalize(call)
		}
		if !callsign.Valid(call) {
			t.println("That is not a valid callsign.")
			continue
		}
		call = callsign.Base(call)
		t.printf("Password: ")
		pw, err := t.readLine(handshakeTimeout)
		if err != nil {
			return false
		}
		u, err := t.srv.store.GetUser(call)
		if err == nil && t.srv.checkPassword(u, pw) {
			t.call, t.admin = call, u.Admin
			return true
		}
		if errors.Is(err, store.ErrNotFound) || (err == nil && len(u.PubKey) == 0) {
			t.println("No account with a password for " + call + ". Register by syncing once with the HamBBS client, or ask the sysop.")
			return false
		}
		t.srv.event("terminal login failed for %s from %s", call, t.c.Remote())
		t.println("Wrong password.")
	}
	return false
}

// command executes one command line; false means disconnect.
func (t *terminal) command(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}
	cmd := strings.ToUpper(fields[0])
	args := fields[1:]
	switch cmd {
	case "H", "?", "HELP":
		t.println(helpText)
	case "B", "BYE", "Q", "QUIT", "EXIT":
		t.printf("73 de %s%s", t.srv.cfg.Callsign, t.nl)
		return false
	case "I", "INFO":
		t.info()
	case "L":
		t.list(store.ListFilter{Call: t.call, Unread: true, Limit: 50}, "No new messages.")
	case "LL":
		n := 20
		if len(args) > 0 {
			if v, err := strconv.Atoi(args[0]); err == nil && v > 0 && v <= 500 {
				n = v
			}
		}
		t.list(store.ListFilter{Call: t.call, Limit: n}, "No messages.")
	case "LM":
		t.list(store.ListFilter{Call: t.call, Mine: true, Limit: 50}, "No personal mail.")
	case "LB":
		f := store.ListFilter{Kind: "B", Limit: 50}
		if len(args) > 0 {
			f.Area = strings.ToUpper(args[0])
		}
		t.list(f, "No bulletins.")
	case "A", "AREAS":
		t.areas()
	case "R", "READ":
		if len(args) == 0 {
			t.println("Usage: R n")
			break
		}
		for _, a := range args {
			t.read(a)
		}
	case "S", "SP", "SEND":
		if len(args) == 0 {
			t.println("Usage: S CALL")
			break
		}
		t.send("P", args[0])
	case "SB":
		if len(args) == 0 {
			t.println("Usage: SB AREA")
			break
		}
		t.send("B", args[0])
	case "K", "KILL":
		if len(args) == 0 {
			t.println("Usage: K n")
			break
		}
		t.kill(args[0])
	default:
		t.printf("Unknown command %q. Type H for help.%s", fields[0], t.nl)
	}
	return true
}

func (t *terminal) info() {
	st, _ := t.srv.store.Stats()
	t.printf("%s, sysop %s. Node %s.%s", t.srv.bannerText(), t.srv.cfg.Callsign, t.srv.cfg.Node, t.nl)
	t.printf("%d users, %d personal messages, %d bulletins. Up since %s.%s",
		st.Users, st.Personal, st.Bulletins, t.srv.started.UTC().Format("2006-01-02 15:04Z"), t.nl)
}

func (t *terminal) unreadPersonal() int {
	ms, err := t.srv.store.ListMessages(store.ListFilter{Call: t.call, Mine: true, Unread: true})
	if err != nil {
		return 0
	}
	return len(ms)
}

func (t *terminal) list(f store.ListFilter, empty string) {
	ms, err := t.srv.store.ListMessages(f)
	if err != nil {
		t.println("Error reading messages.")
		return
	}
	if len(ms) == 0 {
		t.println(empty)
		return
	}
	t.println("  Nr  K To        From      Date  Subject")
	for _, m := range ms {
		to := callsign.ParseAddress(m.To).Name
		t.printf("%5d %s %-9s %-9s %s %s%s", m.Seq, m.Kind, trunc(to, 9), trunc(m.From, 9),
			time.Unix(m.Created, 0).UTC().Format("01-02"), trunc(m.Subject, 40), t.nl)
	}
}

func (t *terminal) areas() {
	as, err := t.srv.store.Areas()
	if err != nil {
		t.println("Error reading areas.")
		return
	}
	if len(as) == 0 {
		t.println("No bulletins yet. Suggested areas: " + strings.Join(t.srv.cfg.Areas, " "))
		return
	}
	for _, a := range as {
		t.printf("%-12s %4d  last %s%s", a.Name, a.Count, a.Last.UTC().Format("2006-01-02"), t.nl)
	}
}

func (t *terminal) lookup(arg string) (store.Message, bool) {
	n, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		t.printf("%q is not a message number.%s", arg, t.nl)
		return store.Message{}, false
	}
	m, err := t.srv.store.MessageBySeq(n)
	if err != nil || !canSee(t.call, m.Message) {
		t.printf("No message %d.%s", n, t.nl)
		return store.Message{}, false
	}
	return m, true
}

func (t *terminal) read(arg string) {
	m, ok := t.lookup(arg)
	if !ok {
		return
	}
	t.printf("Msg #%d  %s%s", m.Seq, kindName(m.Kind), t.nl)
	t.printf("From: %s%s", m.From, t.nl)
	t.printf("To:   %s%s", m.To, t.nl)
	t.printf("Date: %s%s", time.Unix(m.Created, 0).UTC().Format("2006-01-02 15:04Z"), t.nl)
	t.printf("Subj: %s%s", m.Subject, t.nl)
	t.println("")
	t.println(m.Body)
	t.println("[End of message]")
	t.srv.store.MarkRead(t.call, m.ID)
}

func (t *terminal) send(kind, to string) {
	m := proto.Message{Kind: kind, To: to, Created: time.Now().Unix()}
	// Validate the address before asking for the text.
	var err error
	if kind == "P" {
		_, err = callsign.ParsePersonal(to)
	} else {
		_, err = callsign.ParseBulletin(to)
	}
	if err != nil {
		t.println("Cannot send: " + err.Error())
		return
	}
	t.printf("Subject: ")
	subj, err := t.readLine(idleTimeout)
	if err != nil {
		return
	}
	if strings.TrimSpace(subj) == "" {
		t.println("Aborted (empty subject).")
		return
	}
	m.Subject = subj
	t.println("Enter text. End with /EX on a line by itself (or Ctrl-Z). /ABORT cancels.")
	var body []string
	size := 0
	for {
		line, err := t.readLine(idleTimeout)
		if err != nil {
			return
		}
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(trimmed, "/ABORT") {
			t.println("Message aborted.")
			return
		}
		if i := strings.IndexByte(line, 0x1a); i >= 0 {
			body = append(body, line[:i])
			break
		}
		if strings.EqualFold(trimmed, "/EX") {
			break
		}
		size += len(line) + 1
		if t.srv.cfg.MaxMessageBytes > 0 && size > t.srv.cfg.MaxMessageBytes {
			t.println("Message too long, aborted.")
			return
		}
		body = append(body, line)
	}
	m.Body = strings.Join(body, "\n")
	m.ID = proto.NewMessageID(t.call, m.Created)
	nm, err := t.srv.accept(t.call, m, time.Now())
	if err != nil {
		t.println("Not stored: " + err.Error())
		return
	}
	seq, _, err := t.srv.store.AddMessage(nm)
	if err != nil {
		t.println("Not stored: database error.")
		return
	}
	t.srv.store.MarkRead(t.call, nm.ID)
	t.srv.event("%s posted %s to %s: %s", t.call, kindName(nm.Kind), nm.To, nm.Subject)
	t.printf("Stored as message #%d.%s", seq, t.nl)
}

func (t *terminal) kill(arg string) {
	m, ok := t.lookup(arg)
	if !ok {
		return
	}
	mine := m.From == t.call || (m.Kind == "P" && callsign.ParseAddress(m.To).Name == t.call)
	if !mine && !t.admin {
		t.println("You can only delete your own messages or mail addressed to you.")
		return
	}
	if err := t.srv.store.DeleteMessage(m.ID); err != nil {
		t.println("Delete failed.")
		return
	}
	t.srv.event("%s deleted message #%d", t.call, m.Seq)
	t.printf("Message #%d deleted.%s", m.Seq, t.nl)
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
