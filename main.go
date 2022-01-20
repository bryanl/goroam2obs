package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cheggaaa/pb/v3"
)

func main() {
	var ac appConfig
	flag.StringVar(&ac.input, "i", "", "Input file")
	flag.StringVar(&ac.outDir, "d", "", "Output directory")
	flag.Parse()

	if err := run(ac); err != nil {
		log.Print(err)
		flag.Usage()
		os.Exit(1)
	}
}

func run(ac appConfig) error {
	if err := ac.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	pages, err := loadJSON(ac.input)
	if err != nil {
		return fmt.Errorf("load JSON: %w", err)
	}

	for i := range pages {
		for j := range pages[i].Children() {
			pages[i].RawChildren[j].Page = pages[i]
		}
	}

	uidBlock, err := pass1(pages)
	if err != nil {
		return fmt.Errorf("pass1: %w", err)
	}

	referencedUID := map[string]struct{}{}
	if err := pass2(pages, uidBlock, referencedUID); err != nil {
		return fmt.Errorf("pass2: %w", err)
	}

	return pass3(pages, uidBlock, referencedUID, ac.outDir)
}

func pass3(pages []Page, uidBlock map[string]Child, referencedUID map[string]struct{}, outDir string) error {
	bar := pb.StartNew(len(pages))
	for _, page := range pages {
		if page.Title == "" {
			continue
		}

		title := strings.ReplaceAll(page.Title, "[[", "")
		title = strings.ReplaceAll(title, "]]", "")

		dest := filepath.Join(outDir, page.Title+".md")
		if page.IsDaily {
			dest = filepath.Join(outDir, "daily", page.Title+".md")
		}

		dir := filepath.Dir(dest)

		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}

		lines, err := expandChildren(&page, uidBlock, referencedUID, 0)
		if err != nil {
			return err
		}

		data := strings.Join(lines, "\n")

		if err := os.WriteFile(dest, []byte(data), 0644); err != nil {
			return err
		}

		bar.Increment()
	}
	bar.Finish()

	return nil
}

func pass2(pages []Page, uidBlock map[string]Child, referencedUID map[string]struct{}) error {
	fmt.Println("Pass 2: track blockrefs")

	bar := pb.StartNew(len(pages))
	for _, page := range pages {
		_, err := expandChildren(&page, uidBlock, referencedUID, 0)
		if err != nil {
			return fmt.Errorf("pass2: %w", err)
		}
		bar.Increment()
	}
	bar.Finish()

	return nil
}

func expandChildren(parent Parent, uidBlock map[string]Child, referencedUID map[string]struct{}, level int) ([]string, error) {
	var lines []string

	for _, child := range parent.Children() {
		prefix := ""
		if level > 0 {
			prefix = strings.Repeat(" ", 4*level)
		}

		s := child.String
		if child.Heading > 0 {
			prefix = strings.Repeat("#", child.Heading) + " " + prefix
		}

		if len(child.Children()) > 0 && level > 0 {
			prefix += "* "
		}

		postfix := ""
		if _, ok := referencedUID[child.UID]; ok {
			postfix = fmt.Sprintf(" ^%s", child.UID)
		}

		updated, err := replaceBlockRefs(s, uidBlock, referencedUID)
		if err != nil {
			return nil, err
		}

		s = prefix + updated + postfix
		if strings.ContainsRune(s, '\n') {
			s = strings.ReplaceAll(s, "\n", "\n"+prefix) + "\n"
		}

		lines = append(lines, s)

		expanded, err := expandChildren(&child, uidBlock, referencedUID, level+1)
		if err != nil {
			return nil, err
		}

		lines = append(lines, expanded...)
	}

	return lines, nil
}

func replaceBlockRefs(s string, uidBlock map[string]Child, referencedUID map[string]struct{}) (string, error) {
	// need to replay block embeds, block mentions, block refs with some text

	update := s

	regexList := []*regexp.Regexp{reBlockEmbed, reBlockMentions, reBlockRef}

	for {
		var match []int
		for _, re := range regexList {
			match = re.FindStringSubmatchIndex(update)
			if match == nil {
				break
			}

			uid := update[match[4]:match[5]]
			child, ok := uidBlock[uid]
			if !ok {
				fmt.Println("**** did not find uid:", uid)
				continue
			}

			referencedUID[uid] = struct{}{}
			head := update[:match[0]]
			replacement := fmt.Sprintf("%s [[%s#^%s]]", child.String, child.Page.Title, child.UID)
			tail := update[match[1]:]
			update = head + replacement + tail
		}

		if match == nil {
			break
		}
	}

	return replaceDayLinks(update)
}

func replaceDayLinks(in string) (string, error) {
	update := in

	for {
		match := reDayLink.FindStringSubmatchIndex(update)
		if match == nil {
			break
		}

		date := update[match[4]:match[5]]
		obsDate, _, err := parseRoamDate(date)
		if err != nil {
			return "", fmt.Errorf("invalid date %q: %w", date, err)
		}

		head := update[:match[0]] + "[["
		tail := "]]" + update[match[1]:]
		update = head + obsDate + tail
	}

	return update, nil
}

func pass1(pages []Page) (map[string]Child, error) {
	fmt.Println("Pass 1: scan all pages")
	bar := pb.StartNew(len(pages))

	uidBlock := map[string]Child{}

	for i, page := range pages {
		title, err := parsePageDate(&page)
		if err != nil {
			return nil, fmt.Errorf("parse page date: %w", err)
		}
		pages[i].Title = title

		// collect uid
		collectBlocks(uidBlock, &page, page.RawChildren)

		bar.Increment()
	}

	bar.Finish()

	return uidBlock, nil
}

func collectBlocks(uidList map[string]Child, page *Page, children []Child) {
	for _, child := range children {
		child.Page = *page
		uidList[child.UID] = child
		collectBlocks(uidList, page, child.RawChildren)
	}
}

func parsePageDate(page *Page) (string, error) {
	update, ok, err := parseRoamDate(page.Title)
	if err != nil {
		return "", err
	}

	if ok {
		page.IsDaily = true
	}

	return update, nil
}

func parseRoamDate(in string) (string, bool, error) {
	match := reDaily.FindAllStringSubmatch(in, -1)

	if len(match) != 1 {
		return in, false, nil
	}
	row := match[0]
	rawTitle := fmt.Sprintf("%s %s %s", row[1], row[2], row[3])

	t, err := time.Parse(roamDailyLayout, rawTitle)
	if err != nil {
		return "", false, err
	}

	return t.Format(obsDailyLayout), true, nil
}

func loadJSON(jsonPath string) ([]Page, error) {
	f, err := os.Open(jsonPath)
	if err != nil {
		return nil, err
	}

	defer func(f *os.File) {
		err := f.Close()
		if err != nil {

		}
	}(f)

	var pages []Page

	if err := json.NewDecoder(f).Decode(&pages); err != nil {
		return nil, err
	}

	return pages, nil
}

type appConfig struct {
	input  string
	outDir string
}

func (ac *appConfig) Validate() error {
	if ac.input == "" {
		return errors.New("input is blank")
	}

	if ac.outDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		ac.outDir = wd
	}

	return nil
}

type Parent interface {
	Children() []Child
}

type Page struct {
	Title         string  `json:"title"`
	RawChildren   []Child `json:"children"`
	RawCreateTime int     `json:"create-time"`
	CreateEmail   string  `json:"create-email"`
	RawEditTime   int     `json:"edit-time"`
	EditEmail     string  `json:"edit-email"`

	CreateTime time.Time `json:"-"`
	EditTime   time.Time `json:"-"`

	IsDaily bool `json:"-"`
}

func (p *Page) Children() []Child {
	return p.RawChildren
}

var _ json.Unmarshaler = &Page{}
var _ Parent = &Page{}

type dummyPage Page

func (p *Page) UnmarshalJSON(bytes []byte) error {
	d := &dummyPage{}

	if err := json.Unmarshal(bytes, d); err != nil {
		return err
	}

	p.Title = d.Title
	p.RawChildren = d.RawChildren
	p.CreateEmail = d.CreateEmail
	p.EditEmail = d.EditEmail

	if p.RawCreateTime == 0 {
		p.RawCreateTime = int(time.Now().Unix())
	}

	if p.RawEditTime == 0 {
		p.RawEditTime = int(time.Now().Unix())
	}

	p.CreateTime = time.Unix(int64(d.RawCreateTime), 0)
	p.EditTime = time.Unix(int64(d.RawEditTime), 0)

	return nil
}

type Child struct {
	UID           string  `json:"uid"`
	String        string  `json:"string"`
	RawChildren   []Child `json:"children"`
	RawCreateTime int     `json:"create-time"`
	CreateEmail   string  `json:"create-email"`
	RawEditTime   int     `json:"edit-time"`
	EditEmail     string  `json:"edit-email"`
	Heading       int     `json:"heading"`
	Emojis        []Emoji `json:"emojis"`
	TextAlign     string  `json:"text-align"`

	CreateTime time.Time `json:"-"`
	EditTime   time.Time `json:"-"`

	Page Page `json:"-"`
}

var _ json.Unmarshaler = &Child{}
var _ Parent = &Child{}

type dummyChild Child

func (c *Child) Children() []Child {
	return c.RawChildren
}

func (c *Child) UnmarshalJSON(bytes []byte) error {
	d := &dummyChild{}

	if err := json.Unmarshal(bytes, d); err != nil {
		return err
	}
	c.UID = d.UID
	c.String = d.String
	c.RawChildren = d.RawChildren
	c.CreateEmail = d.CreateEmail
	c.EditEmail = d.EditEmail
	c.Heading = d.Heading
	c.Emojis = d.Emojis
	c.TextAlign = d.TextAlign

	if c.RawCreateTime == 0 {
		c.RawCreateTime = int(time.Now().Unix())
	}

	if c.RawEditTime == 0 {
		c.RawEditTime = int(time.Now().Unix())
	}

	c.CreateTime = time.Unix(int64(d.RawCreateTime), 0)
	c.EditTime = time.Unix(int64(d.RawEditTime), 0)

	return nil
}

type Emoji struct {
	Emoji map[string]interface{}   `json:"emoji"`
	Users []map[string]interface{} `json:"users"`
}

var (
	reDaily         = regexp.MustCompile(`^(January|February|March|April|May|June|July|August|September|October|November|December) ([0-9]+)[a-z]{2}, ([0-9]{4})$`)
	reDayLink       = regexp.MustCompile(`(\[\[)([January|February|March|April|May|June|July|August|September|October|November|December [0-9]+[a-z]{2}, [0-9]{4})(\]\])`)
	reBlockEmbed    = regexp.MustCompile(`({{embed: \(\()(.{9})(\)\)}})`)
	reBlockMentions = regexp.MustCompile(`({{mentions: \(\()(.{9})(\)\)}})`)
	reBlockRef      = regexp.MustCompile(`(\(\()(.{9})(\)\))`)
)

const (
	roamDailyLayout = "January _2 2006"
	obsDailyLayout  = "2006-01-02"
)
