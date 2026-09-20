package todo

// Counts returns how many items are pending and how many are done.
func (l List) Counts() (int, int) {
	p := 0
	d := 0
	for i := 0; i < len(l); i++ {
		if l[i].Done == true {
			d = d + 1
		} else {
			p = p + 1
		}
	}
	return p, d
}
