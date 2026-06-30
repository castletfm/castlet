// Transcript ↔ audio sync. Clicking a segment's timecode seeks the player;
// as playback advances the active segment is highlighted and kept in view.
(function () {
  "use strict";
  var audio = document.getElementById("player");
  var list = document.getElementById("segments");
  if (!audio || !list) return;

  var segments = Array.prototype.slice.call(list.querySelectorAll(".segment"));
  if (segments.length === 0) return;

  var starts = segments.map(function (el) {
    return parseFloat(el.getAttribute("data-start")) || 0;
  });

  // Seek when a timecode button is clicked.
  list.addEventListener("click", function (e) {
    var btn = e.target.closest(".seek");
    if (!btn) return;
    var t = parseFloat(btn.getAttribute("data-start"));
    if (!isNaN(t)) {
      audio.currentTime = t;
      audio.play();
    }
  });

  // Binary-search the current segment index for the given time.
  function indexFor(time) {
    var lo = 0, hi = starts.length - 1, ans = 0;
    while (lo <= hi) {
      var mid = (lo + hi) >> 1;
      if (starts[mid] <= time) { ans = mid; lo = mid + 1; }
      else { hi = mid - 1; }
    }
    return ans;
  }

  var current = -1;
  audio.addEventListener("timeupdate", function () {
    var i = indexFor(audio.currentTime);
    if (i === current) return;
    if (current >= 0) segments[current].classList.remove("active");
    segments[i].classList.add("active");
    current = i;
  });
})();
