package oademo

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The organization tree (OA_ORG_FILE): one company -> business line ->
// department -> center -> people tree from which both identity cards pick,
// the OA account card (POST /switch) and the agent card (POST
// /agent-identity). A <select> cannot nest, so each card renders the tree as
// nested lists of <details> with one submit button per person; /tree.js
// (treeScript) adds a search box that filters it. Without the file both cards
// keep their flat <select> (accountGroups, agentPrincipalGroups).

// OrgTreeVersion is the only version LoadOrgTree accepts.
const OrgTreeVersion = "oa-org-tree-v1"

// OrgTree is the organization tree file.
type OrgTree struct {
	Version string  `yaml:"version"`
	Company string  `yaml:"company"`
	Root    OrgNode `yaml:"root"`
}

// OrgNode is a group (Label, Code, Children) or a person (Subject, Name,
// Title, RoleLabel, Department, Center).
type OrgNode struct {
	Kind       string    `yaml:"kind"`
	Label      string    `yaml:"label"`
	Code       string    `yaml:"code"`
	Children   []OrgNode `yaml:"children"`
	Subject    string    `yaml:"subject"`
	Name       string    `yaml:"name"`
	Title      string    `yaml:"title"`
	RoleLabel  string    `yaml:"role_label"`
	Department string    `yaml:"department"`
	Center     string    `yaml:"center"`
}

// orgSubject is the form of a person's subject: a pinyin login name.
var orgSubject = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// LoadOrgTree reads and checks an organization tree file; an empty path
// yields no tree (nil).
func LoadOrgTree(path string) (*OrgTree, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tree OrgTree
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("org tree file %s: %w", path, err)
	}
	if err := tree.validate(); err != nil {
		return nil, fmt.Errorf("org tree file %s: %w", path, err)
	}
	return &tree, nil
}

// validate checks the version, that the root is a group, that every group
// has a label and every person a name and a subject of the form orgSubject,
// that no person has children, and that no subject appears twice.
func (t *OrgTree) validate() error {
	if t.Version != OrgTreeVersion {
		return fmt.Errorf("version must be %q", OrgTreeVersion)
	}
	if t.Root.Kind != "group" {
		return errors.New("root must be a group")
	}
	return t.Root.validate("", map[string]bool{})
}

func (n *OrgNode) validate(at string, seen map[string]bool) error {
	switch n.Kind {
	case "group":
		if strings.TrimSpace(n.Label) == "" {
			return fmt.Errorf("a group under %q has no label", at)
		}
		inner := strings.TrimPrefix(at+" / "+n.Label, " / ")
		for index := range n.Children {
			if err := n.Children[index].validate(inner, seen); err != nil {
				return err
			}
		}
	case "person":
		if !orgSubject.MatchString(n.Subject) {
			return fmt.Errorf("person %q under %q: subject must match %s", n.Subject, at, orgSubject)
		}
		if seen[n.Subject] {
			return fmt.Errorf("person %s appears twice (again under %q)", n.Subject, at)
		}
		seen[n.Subject] = true
		if strings.TrimSpace(n.Name) == "" {
			return fmt.Errorf("person %s under %q has no name", n.Subject, at)
		}
		if len(n.Children) > 0 {
			return fmt.Errorf("person %s under %q has children", n.Subject, at)
		}
	default:
		return fmt.Errorf("a node under %q is of kind %q, not group or person", at, n.Kind)
	}
	return nil
}

// post is what the tree says a person does: the title, else the role label.
func (n OrgNode) post() string {
	if title := strings.TrimSpace(n.Title); title != "" {
		return title
	}
	return strings.TrimSpace(n.RoleLabel)
}

// orgPerson is one person of the tree with the labels of the groups that
// enclose them, the company first.
type orgPerson struct {
	OrgNode
	Path []string
}

// line is how a card's big line names the person: 姓名 · 岗位 · 部门 中心,
// the department and center being the enclosing groups below the business
// line (a leader above them has none).
func (p orgPerson) line() string {
	parts := []string{p.Name}
	if post := p.post(); post != "" {
		parts = append(parts, post)
	}
	if len(p.Path) > 2 {
		parts = append(parts, strings.Join(p.Path[2:], " "))
	}
	return strings.Join(parts, " · ")
}

// search is the text the tree's search box matches a person against (lower
// case): the name, the subject, the title and role label, and the labels of
// the enclosing groups.
func (p orgPerson) search() string {
	words := append([]string{p.Name, p.Subject, p.Title, p.RoleLabel}, p.Path...)
	return searchText(words...)
}

func searchText(words ...string) string {
	kept := make([]string, 0, len(words))
	for _, word := range words {
		if word = strings.TrimSpace(word); word != "" {
			kept = append(kept, word)
		}
	}
	return strings.ToLower(strings.Join(kept, " "))
}

// people indexes the tree's persons by subject.
func (t *OrgTree) people() map[string]orgPerson {
	people := map[string]orgPerson{}
	var walk func(node OrgNode, path []string)
	walk = func(node OrgNode, path []string) {
		if node.Kind == "person" {
			people[node.Subject] = orgPerson{OrgNode: node, Path: path}
			return
		}
		inner := append(append([]string(nil), path...), node.Label)
		for _, child := range node.Children {
			walk(child, inner)
		}
	}
	walk(t.Root, nil)
	return people
}

// orgNodeView is one node of a card's tree: a group (Label, Open, Children)
// or a person, offered as a submit button named Field with Value, or Missing
// (no Value: the card cannot switch to them) and greyed.
type orgNodeView struct {
	Group    bool
	Label    string
	Open     bool
	Children []orgNodeView
	Field    string
	Value    string
	Name     string
	Title    string
	Search   string
	Current  bool
	Missing  bool
}

// otherGroup collects, after the company, what a card offers that the tree
// does not name, so nothing becomes unreachable.
const otherGroup = "其他"

// orgOpenDepth is how deep the groups are open by default: the company and
// its business lines; departments and centers start folded.
const orgOpenDepth = 2

// orgView renders the tree for one card: pick answers, for a person's
// subject, the value their button submits (empty: missing) and whether they
// are the card's current identity.
func orgView(root OrgNode, field string, pick func(subject string) (value string, current bool)) orgNodeView {
	var build func(node OrgNode, depth int, path []string) orgNodeView
	build = func(node OrgNode, depth int, path []string) orgNodeView {
		if node.Kind == "person" {
			person := orgPerson{OrgNode: node, Path: path}
			view := orgNodeView{Field: field, Name: node.Name, Title: node.post(), Search: person.search()}
			view.Value, view.Current = pick(node.Subject)
			view.Missing = view.Value == ""
			view.Current = view.Current && !view.Missing
			return view
		}
		view := orgNodeView{Group: true, Label: node.Label, Open: depth < orgOpenDepth}
		inner := append(append([]string(nil), path...), node.Label)
		for _, child := range node.Children {
			view.Children = append(view.Children, build(child, depth+1, inner))
		}
		return view
	}
	return build(root, 0, nil)
}

// orgAccountTree is the OA card's tree: a person of the tree who is an
// account is a button (the session's account current), the others are
// missing; the accounts the tree does not name follow under otherGroup.
func (s *Server) orgAccountTree(current string) []orgNodeView {
	nodes := []orgNodeView{orgView(s.config.OrgTree.Root, "username", func(subject string) (string, bool) {
		if _, ok := s.users[subject]; ok {
			return subject, subject == current
		}
		return "", false
	})}
	var others []orgNodeView
	for _, option := range s.loginOptions() {
		if _, named := s.orgPeople[option.ID]; named {
			continue
		}
		others = append(others, orgNodeView{Field: "username", Value: option.ID, Name: option.Name, Title: roleLabel(option.Role),
			Current: option.ID == current, Search: searchText(option.Name, option.ID, roleLabel(option.Role), otherGroup)})
	}
	if len(others) > 0 {
		nodes = append(nodes, orgNodeView{Group: true, Label: otherGroup, Open: true, Children: others})
	}
	return nodes
}

// agentTree is the agent card's tree: TaskGate's candidates joined to the
// tree's persons by subject (a subject with several principals keeps the
// first by principal id in the tree), the current principal marked; a person
// without a candidate is missing, and the candidates the tree does not take
// follow under otherGroup, by subject.
func (s *Server) agentTree(candidates []agentCandidate, selected string) []orgNodeView {
	sorted := append([]agentCandidate(nil), candidates...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Subject != sorted[j].Subject {
			return sorted[i].Subject < sorted[j].Subject
		}
		return sorted[i].PrincipalID < sorted[j].PrincipalID
	})
	joined := map[string]agentCandidate{}
	var others []agentCandidate
	for _, candidate := range sorted {
		if _, named := s.orgPeople[candidate.Subject]; named && candidate.PrincipalID != "" {
			if _, taken := joined[candidate.Subject]; !taken {
				joined[candidate.Subject] = candidate
				continue
			}
		}
		others = append(others, candidate)
	}
	nodes := []orgNodeView{orgView(s.config.OrgTree.Root, "principal_id", func(subject string) (string, bool) {
		if candidate, ok := joined[subject]; ok {
			return candidate.PrincipalID, candidate.PrincipalID == selected
		}
		return "", false
	})}
	var rest []orgNodeView
	for _, candidate := range others {
		name := candidate.Subject
		if account, ok := s.users[strings.ToLower(candidate.Subject)]; ok && candidate.Subject != "" {
			name = account.Name
		}
		if name == "" {
			name = candidate.PrincipalID
		}
		job := jobFamilyLabels[candidate.JobFamily]
		if job == "" {
			job = candidate.JobFamily
		}
		leaf := orgNodeView{Field: "principal_id", Value: candidate.PrincipalID, Name: name, Title: job,
			Current: candidate.PrincipalID != "" && candidate.PrincipalID == selected,
			Search:  searchText(name, candidate.Subject, candidate.PrincipalID, job, otherGroup)}
		leaf.Missing = leaf.Value == ""
		rest = append(rest, leaf)
	}
	if len(rest) > 0 {
		nodes = append(nodes, orgNodeView{Group: true, Label: otherGroup, Open: true, Children: rest})
	}
	return nodes
}

// treeScript makes a card's tree searchable: typing filters the persons by
// name, pinyin subject or the labels above them (case-insensitive
// substring), opens the groups holding a match and hides the rest; clearing
// the box restores the groups as they were. Enter picks the single visible
// match. The box is rendered hidden and shown here, so without scripts no
// box can submit the first person of the form by an implicit Enter. Served
// from the OA itself for the Content-Security-Policy, like switchScript.
func treeScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write([]byte(`(function () {
  function each(list, fn) { Array.prototype.forEach.call(list, fn); }
  function setup(tree) {
    var input = tree.querySelector('input[type="search"]');
    if (!input) { return; }
    var leaves = tree.querySelectorAll("li.leaf");
    var groups = tree.querySelectorAll("li.node");
    var saved = null;
    function own(group) { return group.querySelector("details"); }
    function filter() {
      var query = input.value.trim().toLowerCase();
      if (query === "") {
        each(leaves, function (leaf) { leaf.hidden = false; });
        each(groups, function (group, index) {
          group.hidden = false;
          if (saved) { own(group).open = saved[index]; }
        });
        saved = null;
        return;
      }
      if (!saved) {
        saved = [];
        each(groups, function (group) { saved.push(own(group).open); });
      }
      each(leaves, function (leaf) {
        leaf.hidden = (leaf.getAttribute("data-search") || "").toLowerCase().indexOf(query) < 0;
      });
      each(groups, function (group) {
        var matched = group.querySelector("li.leaf:not([hidden])") !== null;
        group.hidden = !matched;
        if (matched) { own(group).open = true; }
      });
    }
    input.hidden = false;
    input.addEventListener("input", filter);
    input.addEventListener("keydown", function (event) {
      if (event.key !== "Enter") { return; }
      event.preventDefault();
      if (event.isComposing || event.keyCode === 229) { return; }
      var shown = [];
      each(leaves, function (leaf) { if (!leaf.hidden) { shown.push(leaf); } });
      if (shown.length !== 1) { return; }
      var button = shown[0].querySelector("button.pick");
      if (button && !button.disabled) { button.click(); }
    });
  }
  document.addEventListener("DOMContentLoaded", function () {
    each(document.querySelectorAll("nav details.tree"), setup);
  });
})();
`))
}
