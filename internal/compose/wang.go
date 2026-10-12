package compose

// KitClass is one compositional piece of a 20-class wang-tile kit. Key is the
// file suffix; Hint is appended to the kit prompt to steer the render toward
// the piece; SeedOffset is the stable additive offset so a kit's classes are
// reproducible from one base seed.
type KitClass struct {
	Key        string `json:"key"`
	SeedOffset int64  `json:"seed_offset"`
	Hint       string `json:"hint"`
}

// KitClasses is the fixed 20-class wang set. Order matters: index 0
// ("line-h") is the kit's primary/preview file, and SeedOffsets are the
// class's identity — never reorder or renumber.
var KitClasses = []KitClass{
	{"line-h", 0, "a straight horizontal line spanning edge to edge, centered vertically"},
	{"line-v", 1, "a straight vertical line spanning edge to edge, centered horizontally"},
	{"corner-out-nw", 2, "an outer corner: the line enters the north edge and exits the west edge, turning through a quarter-circle in the top-left region"},
	{"corner-out-ne", 3, "an outer corner: the line enters the north edge and exits the east edge, turning through a quarter-circle in the top-right region"},
	{"corner-out-sw", 4, "an outer corner: the line enters the south edge and exits the west edge, turning through a quarter-circle in the bottom-left region"},
	{"corner-out-se", 5, "an outer corner: the line enters the south edge and exits the east edge, turning through a quarter-circle in the bottom-right region"},
	{"corner-in-nw", 6, "an inner corner: lines run along the top edge and the left edge, joining with a concave quarter-circle notch facing the bottom-right"},
	{"corner-in-ne", 7, "an inner corner joining the top edge and the right edge with a concave quarter-circle notch facing the bottom-left"},
	{"corner-in-sw", 8, "an inner corner joining the bottom edge and the left edge with a concave quarter-circle notch facing the top-right"},
	{"corner-in-se", 9, "an inner corner joining the bottom edge and the right edge with a concave quarter-circle notch facing the top-left"},
	{"t-e", 10, "a T junction with three branches meeting at the center and the stem exiting the east edge"},
	{"t-w", 11, "a T junction with three branches meeting at the center and the stem exiting the west edge"},
	{"t-n", 12, "a T junction with three branches meeting at the center and the stem exiting the north edge"},
	{"t-s", 13, "a T junction with three branches meeting at the center and the stem exiting the south edge"},
	{"cross", 14, "a four-way cross junction, one branch exiting each of the four edges"},
	{"cap-n", 15, "a single line entering from the north edge and terminating at a rounded endpoint in the tile center"},
	{"cap-s", 16, "a single line entering from the south edge and terminating at a rounded endpoint in the tile center"},
	{"cap-e", 17, "a single line entering from the east edge and terminating at a rounded endpoint in the tile center"},
	{"cap-w", 18, "a single line entering from the west edge and terminating at a rounded endpoint in the tile center"},
	{"single", 19, "a short standalone motif centered in the tile, touching none of the edges"},
}
