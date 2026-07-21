package utils

// DFANode DFA 敏感词过滤 Trie 树节点
type DFANode struct {
	Children map[rune]*DFANode
	IsEnd    bool
}

func NewDFANode() *DFANode {
	return &DFANode{Children: make(map[rune]*DFANode)}
}

// SensitiveFilter DFA 敏感词过滤器，O(n) 时间复杂度
type SensitiveFilter struct {
	root *DFANode
}

func NewSensitiveFilter(words []string) *SensitiveFilter {
	f := &SensitiveFilter{root: NewDFANode()}
	for _, w := range words {
		f.AddWord(w)
	}
	return f
}

func (f *SensitiveFilter) AddWord(word string) {
	node := f.root
	for _, ch := range word {
		if node.Children[ch] == nil {
			node.Children[ch] = NewDFANode()
		}
		node = node.Children[ch]
	}
	node.IsEnd = true
}

func (f *SensitiveFilter) Contains(text string) bool {
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		node := f.root
		for j := i; j < len(runes); j++ {
			ch := runes[j]
			if node.Children[ch] == nil {
				break
			}
			node = node.Children[ch]
			if node.IsEnd {
				return true
			}
		}
	}
	return false
}

// Replace 将敏感词替换为 ***
func (f *SensitiveFilter) Replace(text string) string {
	runes := []rune(text)
	marks := make([]bool, len(runes))

	for i := 0; i < len(runes); i++ {
		node := f.root
		endPos := -1
		for j := i; j < len(runes); j++ {
			ch := runes[j]
			if node.Children[ch] == nil {
				break
			}
			node = node.Children[ch]
			if node.IsEnd {
				endPos = j
			}
		}
		if endPos >= 0 {
			for k := i; k <= endPos; k++ {
				marks[k] = true
			}
		}
	}

	result := make([]rune, 0, len(runes))
	for i, r := range runes {
		if marks[i] {
			if i == 0 || !marks[i-1] {
				result = append(result, []rune("***")...)
			}
		} else {
			result = append(result, r)
		}
	}
	return string(result)
}
