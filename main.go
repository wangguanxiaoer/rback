package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/emicklei/dot"
	"github.com/mhausenblas/kubecuddler"
)

type Rback struct {
	config Config
}

type Config struct {
	renderRules     bool
	renderBindings  bool
	namespace       string
	ignoredPrefixes []string
	resourceKind    string
	resourceNames   []string
}

type Permissions struct {
	ServiceAccounts     map[string][]string
	Roles               map[string][]string
	ClusterRoles        []string
	RoleBindings        map[string][]string
	ClusterRoleBindings []string
}

func main() {

	config := Config{}
	flag.BoolVar(&config.renderBindings, "render-bindings", true, "Whether to render (Cluster)RoleBindings as graph nodes")
	flag.BoolVar(&config.renderRules, "render-rules", true, "Whether to render RBAC rules (e.g. \"get pods\") or not")
	flag.StringVar(&config.namespace, "n", "", "The namespace to render")

	var ignoredPrefixes string
	flag.StringVar(&ignoredPrefixes, "ignore-prefixes", "system:", "Comma-delimited list of (Cluster)Role(Binding) prefixes to ignore ('none' to not ignore anything)")
	flag.Parse()

	if flag.NArg() > 0 {
		config.resourceKind = normalizeKind(flag.Arg(0))
	}
	if flag.NArg() > 1 {
		config.resourceNames = flag.Args()[1:]
	}

	if ignoredPrefixes != "none" {
		config.ignoredPrefixes = strings.Split(ignoredPrefixes, ",")
	}

	rback := Rback{config: config}

	p, err := rback.getPermissions()
	if err != nil {
		fmt.Printf("Can't query permissions due to :%v", err)
		os.Exit(-1)
	}
	g := rback.genGraph(p)
	fmt.Println(g.String())
}

var kindMap = map[string]string{
	"sa":              "serviceaccount",
	"serviceaccounts": "serviceaccount",
}

func normalizeKind(kind string) string {
	kind = strings.ToLower(kind)
	entry, exists := kindMap[kind]
	if exists {
		return entry
	}
	return kind
}

func (r *Rback) shouldIgnore(name string) bool {
	for _, prefix := range r.config.ignoredPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// getServiceAccounts retrieves data about service accounts across all namespaces
func (r *Rback) getServiceAccounts(namespace string, saNames []string) (serviceAccounts map[string][]string, err error) {
	serviceAccounts = make(map[string][]string)
	var args []string
	if namespace == "" {
		args = []string{"sa", "--all-namespaces", "--output", "json"}
	} else if len(saNames) == 0 {
		args = []string{"sa", "-n", namespace, "--output", "json"}
	} else {
		args = append([]string{"sa", "-n", namespace, "--output", "json"}, saNames...)
	}
	res, err := kubecuddler.Kubectl(true, true, "", "get", args...)
	if err != nil {
		return serviceAccounts, err
	}

	var d map[string]interface{}
	b := []byte(res)
	err = json.Unmarshal(b, &d)
	if err != nil {
		return serviceAccounts, err
	}

	if d["kind"] != "List" {
		namespacedName := getNamespacedName(d)
		serviceAccounts[namespacedName.namespace] = append(serviceAccounts[namespacedName.namespace], namespacedName.name)
	} else {
		saitems := d["items"].([]interface{})
		for _, sa := range saitems {
			serviceaccount := sa.(map[string]interface{})
			namespacedName := getNamespacedName(serviceaccount)
			serviceAccounts[namespacedName.namespace] = append(serviceAccounts[namespacedName.namespace], namespacedName.name)
		}
	}
	return serviceAccounts, nil
}

func getNamespacedName(obj map[string]interface{}) NamespacedName {
	metadata := obj["metadata"].(map[string]interface{})
	ns := metadata["namespace"]
	name := metadata["name"]
	return NamespacedName{ns.(string), name.(string)}
}

// getRoles retrieves data about roles across all namespaces
func (r *Rback) getRoles() (result map[string][]string, err error) {
	return r.getNamespacedResources("roles")
}

// getRoleBindings retrieves data about roles across all namespaces
func (r *Rback) getRoleBindings() (result map[string][]string, err error) {
	return r.getNamespacedResources("rolebindings")
}

func (r *Rback) getNamespacedResources(kind string) (result map[string][]string, err error) {
	res, err := kubecuddler.Kubectl(true, true, "", "get", kind, "--all-namespaces", "--output", "json")
	result = make(map[string][]string)
	if err != nil {
		return result, err
	}
	var d map[string]interface{}
	b := []byte(res)
	err = json.Unmarshal(b, &d)
	if err != nil {
		return result, err
	}
	items := d["items"].([]interface{})
	for _, i := range items {
		item := i.(map[string]interface{})
		metadata := item["metadata"].(map[string]interface{})
		name := metadata["name"]
		ns := metadata["namespace"]
		if !r.shouldIgnore(name.(string)) {
			itemJson, _ := struct2json(item)
			result[ns.(string)] = append(result[ns.(string)], itemJson)
		}
	}
	return result, nil
}

// getClusterRoles retrieves data about cluster roles
func (r *Rback) getClusterRoles() (result []string, err error) {
	return r.getClusterScopedResources("clusterroles")
}

// getClusterRoleBindings retrieves data about cluster role bindings
func (r *Rback) getClusterRoleBindings() (result []string, err error) {
	return r.getClusterScopedResources("clusterrolebindings")

}

func (r *Rback) getClusterScopedResources(kind string) (result []string, err error) {
	result = []string{}
	res, err := kubecuddler.Kubectl(true, true, "", "get", kind, "--output", "json")
	if err != nil {
		return result, err
	}
	var d map[string]interface{}
	b := []byte(res)
	err = json.Unmarshal(b, &d)
	if err != nil {
		return result, err
	}
	items := d["items"].([]interface{})
	for _, i := range items {
		item := i.(map[string]interface{})
		metadata := item["metadata"].(map[string]interface{})
		name := metadata["name"]
		if !r.shouldIgnore(name.(string)) {
			itemJson, _ := struct2json(item)
			result = append(result, itemJson)
		}
	}
	return result, nil
}

// getPermissions retrieves data about all access control related data
// from service accounts to roles and bindings, both namespaced and the
// cluster level.
func (r *Rback) getPermissions() (Permissions, error) {
	p := Permissions{}
	saNames := []string{}
	if r.config.resourceKind == "serviceaccount" {
		saNames = r.config.resourceNames
	}
	sa, err := r.getServiceAccounts(r.config.namespace, saNames)
	if err != nil {
		return p, err
	}
	p.ServiceAccounts = sa
	roles, err := r.getRoles()
	if err != nil {
		return p, err
	}
	p.Roles = roles
	rb, err := r.getRoleBindings()
	if err != nil {
		return p, err
	}
	p.RoleBindings = rb
	cr, err := r.getClusterRoles()
	if err != nil {
		return p, err
	}
	p.ClusterRoles = cr
	crb, err := r.getClusterRoleBindings()
	if err != nil {
		return p, err
	}
	p.ClusterRoleBindings = crb
	return p, nil
}

type BindingAndRole struct {
	binding NamespacedName
	role    NamespacedName
}

type NamespacedName struct {
	namespace string
	name      string
}

// lookupBindingsAndRoles lists bindings & roles for a given service account
func (r *Rback) lookupBindingsAndRoles(bindings []string, saName, saNamespace string) (roles []BindingAndRole, err error) {
	for _, rb := range bindings {
		var binding map[string]interface{}
		b := []byte(rb)
		err = json.Unmarshal(b, &binding)
		if err != nil {
			return roles, err
		}

		metadata := binding["metadata"].(map[string]interface{})
		bindingName := metadata["name"].(string)
		bindingNs := ""
		if metadata["namespace"] != nil {
			bindingNs = metadata["namespace"].(string)
		}

		roleRef := binding["roleRef"].(map[string]interface{})
		roleName := roleRef["name"].(string)
		roleNs := ""
		if roleRef["namespace"] != nil {
			roleNs = roleRef["namespace"].(string)
		}

		if binding["subjects"] != nil {
			subjects := binding["subjects"].([]interface{})
			for _, subject := range subjects {
				s := subject.(map[string]interface{})
				if s["name"] == saName && s["namespace"] == saNamespace {
					roles = append(roles, BindingAndRole{
						binding: NamespacedName{bindingNs, bindingName},
						role:    NamespacedName{roleNs, roleName},
					})
				}
			}
		}
	}
	return roles, nil
}

// lookupResources lists resources referenced in a role.
// if namespace is empty then the scope is cluster-wide.
func (r *Rback) lookupResources(namespace, role string, p Permissions) (rules string, err error) {
	if namespace != "" { // look up in roles
		rules, err = findAccessRules(p.Roles[namespace], role)
		if err != nil {
			return "", err
		}
	}
	// ... otherwise, look up in cluster roles:
	clusterRules, err := findAccessRules(p.ClusterRoles, role)
	if err != nil {
		return "", err
	}
	return clusterRules + rules, nil
}

func findAccessRules(roles []string, roleName string) (resources string, err error) {
	for _, roleJson := range roles {
		var role map[string]interface{}
		b := []byte(roleJson)
		err = json.Unmarshal(b, &role)
		if err != nil {
			return "", err
		}
		metadata := role["metadata"].(map[string]interface{})
		name := metadata["name"]
		if name == roleName {
			rules := role["rules"].([]interface{})
			for _, rule := range rules {
				r := rule.(map[string]interface{})
				resources += toHumanReadableRule(r) + "\n"
			}
		}
	}
	return resources, nil
}

func toHumanReadableRule(rule map[string]interface{}) string {
	line := toString(rule["verbs"])
	resourceKinds := toString(rule["resources"])
	if resourceKinds != "" {
		line += fmt.Sprintf(` %v`, resourceKinds)
	}
	resourceNames := toString(rule["resourceNames"])
	if resourceNames != "" {
		line += fmt.Sprintf(` "%v"`, resourceNames)
	}
	nonResourceURLs := toString(rule["nonResourceURLs"])
	if nonResourceURLs != "" {
		line += fmt.Sprintf(` %v`, nonResourceURLs)
	}
	apiGroups := toString(rule["apiGroups"])
	if apiGroups != "" {
		line += fmt.Sprintf(` (%v)`, apiGroups)
	}
	return line
}

func toString(values interface{}) string {
	if values == nil {
		return ""
	}
	var strs []string
	for _, v := range values.([]interface{}) {
		strs = append(strs, v.(string))
	}
	return strings.Join(strs, ",")
}

func (r *Rback) genGraph(p Permissions) *dot.Graph {
	g := dot.NewGraph(dot.Directed)
	g.Attr("newrank", "true") // global rank instead of per-subgraph (ensures access rules are always in the same place (at bottom))
	r.renderLegend(g)

	for ns, serviceaccounts := range p.ServiceAccounts {
		gns := g.Subgraph(ns, dot.ClusterOption{})
		gns.Attr("style", "dashed")

		for _, sa := range serviceaccounts {
			sanode := newServiceAccountNode(gns, sa)
			// cluster roles:
			croles, err := r.lookupBindingsAndRoles(p.ClusterRoleBindings, sa, ns)
			if err != nil {
				fmt.Printf("Can't look up cluster roles due to: %v", err)
				os.Exit(-2)
			}
			for _, crole := range croles {
				r.renderRole(g, crole.binding, crole.role, sanode, p)
			}
			// roles:
			roles, err := r.lookupBindingsAndRoles(p.RoleBindings[ns], sa, ns)
			if err != nil {
				fmt.Printf("Can't look up roles due to: %v", err)
				os.Exit(-2)
			}
			for _, role := range roles {
				r.renderRole(gns, role.binding, role.role, sanode, p)
			}

		}
	}
	return g
}

func (r *Rback) renderLegend(g *dot.Graph) {
	legend := g.Subgraph("LEGEND", dot.ClusterOption{})

	namespace := legend.Subgraph("Namespace", dot.ClusterOption{})
	namespace.Attr("style", "dashed")

	sa := newServiceAccountNode(namespace, "ServiceAccount")

	role := newRoleNode(namespace, "ns", "Role")
	clusterRoleBoundLocally := newClusterRoleNode(namespace, "ns", "ClusterRole") // bound by (namespaced!) RoleBinding
	clusterrole := newClusterRoleNode(legend, "", "ClusterRole")

	if r.config.renderBindings {
		roleBinding := newRoleBindingNode(namespace, "RoleBinding")
		sa.Edge(roleBinding).Edge(role)

		roleBinding2 := newRoleBindingNode(namespace, "RoleBinding-to-ClusterRole")
		roleBinding2.Attr("label", "RoleBinding")
		sa.Edge(roleBinding2).Edge(clusterRoleBoundLocally)

		clusterRoleBinding := newClusterRoleBindingNode(legend, "ClusterRoleBinding")
		sa.Edge(clusterRoleBinding).Edge(clusterrole)
	} else {
		legend.Edge(sa, role, "RoleBinding")
		legend.Edge(sa, clusterrole, "ClusterRoleBinding")
		legend.Edge(sa, clusterRoleBoundLocally, "RoleBinding")
	}

	if r.config.renderRules {
		nsrules := newRulesNode(namespace, "ns", "Role", "Namespace-scoped\naccess rules")
		legend.Edge(role, nsrules)

		nsrules2 := newRulesNode(namespace, "ns", "ClusterRole", "Namespace-scoped access rules From ClusterRole")
		nsrules2.Attr("label", "Namespace-scoped\naccess rules")
		legend.Edge(clusterRoleBoundLocally, nsrules2)

		clusterrules := newRulesNode(legend, "", "ClusterRole", "Cluster-scoped\naccess rules")
		legend.Edge(clusterrole, clusterrules)
	}
}

func (r *Rback) renderRole(g *dot.Graph, binding, role NamespacedName, saNode dot.Node, p Permissions) {
	var roleNode dot.Node

	isClusterRole := role.namespace == ""
	if isClusterRole {
		roleNode = newClusterRoleNode(g, binding.namespace, role.name)
	} else {
		roleNode = newRoleNode(g, binding.namespace, role.name)
	}

	if r.config.renderBindings {
		var roleBindingNode dot.Node
		isClusterRoleBinding := binding.namespace == ""
		if isClusterRoleBinding {
			roleBindingNode = newClusterRoleBindingNode(g, binding.name)
		} else {
			roleBindingNode = newRoleBindingNode(g, binding.name)
		}
		saNode.Edge(roleBindingNode).Edge(roleNode)
	} else {
		g.Edge(saNode, roleNode, binding.name)
	}

	if r.config.renderRules {
		rules, err := r.lookupResources(binding.namespace, role.name, p)
		if err != nil {
			fmt.Printf("Can't look up entities and resources due to: %v", err)
			os.Exit(-3)
		}
		if rules != "" {
			resnode := newRulesNode(g, binding.namespace, role.name, rules)
			g.Edge(roleNode, resnode)
		}
	}
}

// struct2json turns a map into a JSON string
func struct2json(s map[string]interface{}) (string, error) {
	str, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(str), nil
}

func newServiceAccountNode(g *dot.Graph, name string) dot.Node {
	return g.Node("sa-"+name).
		Box().
		Attr("label", name).
		Attr("style", "filled").
		Attr("fillcolor", "#2f6de1").
		Attr("fontcolor", "#f0f0f0")
}

func newRoleBindingNode(g *dot.Graph, name string) dot.Node {
	return g.Node("rb-"+name).
		Attr("label", name).
		Attr("shape", "octagon").
		Attr("style", "filled").
		Attr("fillcolor", "#ffcc00").
		Attr("fontcolor", "#030303")
}

func newClusterRoleBindingNode(g *dot.Graph, name string) dot.Node {
	return g.Node("crb-"+name).
		Attr("label", name).
		Attr("shape", "doubleoctagon").
		Attr("style", "filled").
		Attr("fillcolor", "#ffcc00").
		Attr("fontcolor", "#030303")
}

func newRoleNode(g *dot.Graph, namespace, name string) dot.Node {
	return g.Node("r-"+namespace+"/"+name).
		Attr("label", name).
		Attr("shape", "octagon").
		Attr("style", "filled").
		Attr("fillcolor", "#ff9900").
		Attr("fontcolor", "#030303")
}

func newClusterRoleNode(g *dot.Graph, namespace, name string) dot.Node {
	return g.Node("cr-"+namespace+"/"+name).
		Attr("label", name).
		Attr("shape", "doubleoctagon").
		Attr("style", "filled").
		Attr("fillcolor", "#ff9900").
		Attr("fontcolor", "#030303")
}

func newRulesNode(g *dot.Graph, namespace, roleName, rules string) dot.Node {
	rules = strings.ReplaceAll(rules, `\`, `\\`)
	rules = strings.ReplaceAll(rules, "\n", `\l`) // left-justify text
	rules = strings.ReplaceAll(rules, `"`, `\"`)  // using Literal, so we need to escape quotes
	return g.Node("rules-"+namespace+"/"+roleName).
		Attr("label", dot.Literal(`"`+rules+`"`)).
		Attr("shape", "note")
}
