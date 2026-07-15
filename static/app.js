(function () {
  var canvas = document.getElementById('sigpad');
  if (!canvas) return;

  var ctx = canvas.getContext('2d');
  var hint = document.getElementById('sigHint');
  var dataInput = document.getElementById('sigData');
  var clearBtn = document.getElementById('sigClear');
  var drawing = false;
  var hasInk = false;

  function resize() {
    var ratio = window.devicePixelRatio || 1;
    var rect = canvas.getBoundingClientRect();
    canvas.width = rect.width * ratio;
    canvas.height = rect.height * ratio;
    ctx.scale(ratio, ratio);
    ctx.lineWidth = 2.5;
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';
    ctx.strokeStyle = '#161a1f';
  }
  resize();

  function pos(e) {
    var rect = canvas.getBoundingClientRect();
    return { x: e.clientX - rect.left, y: e.clientY - rect.top };
  }

  canvas.addEventListener('pointerdown', function (e) {
    drawing = true;
    hasInk = true;
    if (hint) hint.style.display = 'none';
    canvas.setPointerCapture(e.pointerId);
    var p = pos(e);
    ctx.beginPath();
    ctx.moveTo(p.x, p.y);
  });

  canvas.addEventListener('pointermove', function (e) {
    if (!drawing) return;
    var p = pos(e);
    ctx.lineTo(p.x, p.y);
    ctx.stroke();
  });

  ['pointerup', 'pointercancel', 'pointerleave'].forEach(function (evt) {
    canvas.addEventListener(evt, function () { drawing = false; });
  });

  clearBtn.addEventListener('click', function () {
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    hasInk = false;
    if (hint) hint.style.display = '';
  });

  canvas.closest('form').addEventListener('submit', function () {
    dataInput.value = hasInk ? canvas.toDataURL('image/png') : '';
  });
})();
