"""A fake ``QtCore`` and ``QtWidgets``, with only what c4_katana's dialog
uses, behaving as Qt does where it matters to the dialog:

- signals an artist causes (``textEdited``, ``clicked``, ``activated``) are
  not emitted when code sets a value; ``valueChanged``, ``textChanged`` and
  ``toggled`` are, as in Qt, so the dialog must guard against its own edits;
- a ``QTimer`` runs only when a test fires it (``fire_timers``), as Qt's
  event loop would;
- every widget made is kept in ``created``, so a test can read every label,
  text and tooltip the artist would.
"""

import types
from typing import Any, Callable, List, Optional

created: List["QWidget"] = []
timers: List["QTimer"] = []


class _State:
    def __init__(self):
        self.cancel = False
        self.next_filename = ""
        self.asked: List[dict] = []
        self.progress: List["QProgressDialog"] = []


state = _State()


def reset() -> None:
    global state
    state = _State()
    del created[:]
    del timers[:]


class Signal:
    def __init__(self):
        self._slots: List[Callable] = []

    def connect(self, fn: Callable) -> None:
        self._slots.append(fn)

    def disconnect(self, fn: Optional[Callable] = None) -> None:
        self._slots = [] if fn is None else [s for s in self._slots if s is not fn]

    def emit(self, *args) -> None:
        for fn in list(self._slots):
            fn(*args)


class QObject:
    def __init__(self, parent: Any = None):
        self._parent = parent
        self._name = ""
        self.deleted = False

    def setObjectName(self, name: str) -> None:  # noqa: N802
        self._name = name

    def objectName(self) -> str:  # noqa: N802
        return self._name

    def setParent(self, parent: Any) -> None:  # noqa: N802
        self._parent = parent

    def parent(self):
        return self._parent

    def deleteLater(self) -> None:  # noqa: N802
        self.deleted = True


class QTimer(QObject):
    def __init__(self, parent: Any = None):
        super().__init__(parent)
        self.timeout = Signal()
        self._active = False
        self.interval = 0
        timers.append(self)

    def start(self, ms: int = 0) -> None:
        self._active, self.interval = True, ms

    def stop(self) -> None:
        self._active = False

    def isActive(self) -> bool:  # noqa: N802
        return self._active

    def setInterval(self, ms: int) -> None:  # noqa: N802
        self.interval = ms


def fire_timers() -> int:
    """Qt's event loop: every running timer times out once."""
    n = 0
    for t in list(timers):
        if t.isActive() and not t.deleted:
            t.timeout.emit()
            n += 1
    return n


class Qt:
    AlignLeft = 1
    AlignTop = 32
    NonModal = 0
    WindowModal = 1


QtCore = types.ModuleType("QtCore")
QtCore.QObject, QtCore.QTimer, QtCore.Qt, QtCore.Signal = QObject, QTimer, Qt, Signal


# --- widgets ------------------------------------------------------------------

class QWidget(QObject):
    def __init__(self, parent: Any = None):
        super().__init__(parent)
        self._enabled = True
        self._visible = True
        self._tooltip = ""
        self._layout: Optional["QLayout"] = None
        self._style = ""
        created.append(self)

    def setEnabled(self, on: bool) -> None:  # noqa: N802
        self._enabled = bool(on)

    def isEnabled(self) -> bool:  # noqa: N802
        return self._enabled

    def setVisible(self, on: bool) -> None:  # noqa: N802
        self._visible = bool(on)

    def isVisible(self) -> bool:  # noqa: N802
        return self._visible

    def isHidden(self) -> bool:  # noqa: N802
        return not self._visible

    def setToolTip(self, t: str) -> None:  # noqa: N802
        self._tooltip = t

    def toolTip(self) -> str:  # noqa: N802
        return self._tooltip

    def setLayout(self, layout: "QLayout") -> None:  # noqa: N802
        self._layout = layout
        layout._owner = self

    def layout(self):
        return self._layout

    def setStyleSheet(self, s: str) -> None:  # noqa: N802
        self._style = s

    def setMinimumWidth(self, w: int) -> None:  # noqa: N802
        pass

    def setMinimumHeight(self, h: int) -> None:  # noqa: N802
        pass

    def setMaximumHeight(self, h: int) -> None:  # noqa: N802
        pass

    def children_in_order(self) -> List["QWidget"]:
        return self._layout.widgets() if self._layout is not None else []

    def texts(self) -> List[str]:
        return [self._tooltip]


class QDialog(QWidget):
    def __init__(self, parent: Any = None):
        super().__init__(parent)
        self._title = ""
        self.shown = 0
        self.closed = 0

    def setWindowTitle(self, t: str) -> None:  # noqa: N802
        self._title = t

    def windowTitle(self) -> str:  # noqa: N802
        return self._title

    def show(self) -> None:
        self.shown += 1
        self._visible = True

    def raise_(self) -> None:
        pass

    def activateWindow(self) -> None:  # noqa: N802
        pass

    def close(self) -> bool:
        self.closed += 1
        self._visible = False
        return True

    def texts(self) -> List[str]:
        return [self._title, self._tooltip]


class QLabel(QWidget):
    def __init__(self, text: str = "", parent: Any = None):
        super().__init__(parent)
        self._text = text

    def setText(self, t: str) -> None:  # noqa: N802
        self._text = t

    def text(self) -> str:
        return self._text

    def setWordWrap(self, on: bool) -> None:  # noqa: N802
        pass

    def texts(self) -> List[str]:
        return [self._text, self._tooltip]


class QLineEdit(QWidget):
    def __init__(self, text: str = "", parent: Any = None):
        super().__init__(parent)
        self._text = text
        self._placeholder = ""
        self.textEdited = Signal()
        self.textChanged = Signal()
        self.editingFinished = Signal()

    def setText(self, t: str) -> None:  # noqa: N802
        if t != self._text:
            self._text = t
            self.textChanged.emit(t)

    def text(self) -> str:
        return self._text

    def setPlaceholderText(self, t: str) -> None:  # noqa: N802
        self._placeholder = t

    def setReadOnly(self, on: bool) -> None:  # noqa: N802
        pass

    def texts(self) -> List[str]:
        return [self._text, self._placeholder, self._tooltip]


class QPlainTextEdit(QWidget):
    def __init__(self, text: str = "", parent: Any = None):
        super().__init__(parent)
        self._text = text
        self.read_only = False
        self.textChanged = Signal()

    def setPlainText(self, t: str) -> None:  # noqa: N802
        self._text = t
        self.textChanged.emit()

    def toPlainText(self) -> str:  # noqa: N802
        return self._text

    def setReadOnly(self, on: bool) -> None:  # noqa: N802
        self.read_only = bool(on)

    def setLineWrapMode(self, mode: Any) -> None:  # noqa: N802
        pass

    def texts(self) -> List[str]:
        return [self._text, self._tooltip]


class QComboBox(QWidget):
    def __init__(self, parent: Any = None):
        super().__init__(parent)
        self._items: List[str] = []
        self._index = -1
        self.activated = Signal()
        self.currentIndexChanged = Signal()

    def addItems(self, items) -> None:  # noqa: N802
        self._items += [str(i) for i in items]
        if self._index < 0 and self._items:
            self.setCurrentIndex(0)

    def addItem(self, item: str) -> None:  # noqa: N802
        self.addItems([item])

    def clear(self) -> None:
        self._items, self._index = [], -1
        self.currentIndexChanged.emit(-1)

    def count(self) -> int:
        return len(self._items)

    def itemText(self, i: int) -> str:  # noqa: N802
        return self._items[i]

    def items(self) -> List[str]:
        return list(self._items)

    def findText(self, t: str) -> int:  # noqa: N802
        return self._items.index(t) if t in self._items else -1

    def setCurrentIndex(self, i: int) -> None:  # noqa: N802
        if i != self._index:
            self._index = i
            self.currentIndexChanged.emit(i)

    def currentIndex(self) -> int:  # noqa: N802
        return self._index

    def currentText(self) -> str:  # noqa: N802
        return self._items[self._index] if 0 <= self._index < len(self._items) else ""

    def texts(self) -> List[str]:
        return self._items + [self._tooltip]


class QAbstractButton(QWidget):
    def __init__(self, text: str = "", parent: Any = None):
        super().__init__(parent)
        self._text = text
        self._checkable = False
        self._checked = False
        self.clicked = Signal()
        self.toggled = Signal()

    def setText(self, t: str) -> None:  # noqa: N802
        self._text = t

    def text(self) -> str:
        return self._text

    def setCheckable(self, on: bool) -> None:  # noqa: N802
        self._checkable = bool(on)

    def setChecked(self, on: bool) -> None:  # noqa: N802
        if bool(on) != self._checked:
            self._checked = bool(on)
            self.toggled.emit(self._checked)

    def isChecked(self) -> bool:  # noqa: N802
        return self._checked

    def texts(self) -> List[str]:
        return [self._text, self._tooltip]


class QPushButton(QAbstractButton):
    pass


class QToolButton(QAbstractButton):
    pass


class QCheckBox(QAbstractButton):
    def __init__(self, text: str = "", parent: Any = None):
        super().__init__(text, parent)
        self._checkable = True


class QAbstractSpinBox(QWidget):
    def __init__(self, parent: Any = None):
        super().__init__(parent)
        self._value: Any = 0
        self._min: Any = 0
        self._max: Any = 99
        self.valueChanged = Signal()

    def setRange(self, lo, hi) -> None:  # noqa: N802
        self._min, self._max = lo, hi

    def minimum(self):
        return self._min

    def maximum(self):
        return self._max

    def setValue(self, v) -> None:  # noqa: N802
        v = min(max(v, self._min), self._max)
        if v != self._value:
            self._value = v
            self.valueChanged.emit(v)

    def value(self):
        return self._value


class QSpinBox(QAbstractSpinBox):
    def setValue(self, v) -> None:  # noqa: N802
        super().setValue(int(v))


class QDoubleSpinBox(QAbstractSpinBox):
    def __init__(self, parent: Any = None):
        super().__init__(parent)
        self._value = 0.0

    def setDecimals(self, n: int) -> None:  # noqa: N802
        pass

    def setValue(self, v) -> None:  # noqa: N802
        super().setValue(float(v))


class QGroupBox(QWidget):
    def __init__(self, title: str = "", parent: Any = None):
        super().__init__(parent)
        self._title = title

    def title(self) -> str:
        return self._title

    def texts(self) -> List[str]:
        return [self._title, self._tooltip]


# --- layouts --------------------------------------------------------------------

class QLayout(QObject):
    def __init__(self, parent: Any = None):
        super().__init__(parent)
        self._items: List[Any] = []     # widgets, layouts, and (label, field) rows
        self._owner = None
        if isinstance(parent, QWidget):
            parent.setLayout(self)

    def addWidget(self, w: QWidget, *a) -> None:  # noqa: N802
        self._items.append(w)

    def insertWidget(self, i: int, w: QWidget, *a) -> None:  # noqa: N802
        self._items.insert(i, w)

    def removeWidget(self, w: QWidget) -> None:  # noqa: N802
        self._items = [x for x in self._items if x is not w]

    def addLayout(self, layout: "QLayout", *a) -> None:  # noqa: N802
        self._items.append(layout)

    def addStretch(self, *a) -> None:  # noqa: N802
        pass

    def setContentsMargins(self, *a) -> None:  # noqa: N802
        pass

    def setSpacing(self, *a) -> None:  # noqa: N802
        pass

    def indexOf(self, w: Any) -> int:  # noqa: N802
        for i, x in enumerate(self._items):
            if x is w:
                return i
        return -1

    def widgets(self) -> List[QWidget]:
        """Every widget under this layout, in order, all the way down."""
        out: List[QWidget] = []
        for x in self._items:
            for y in (x if isinstance(x, tuple) else (x,)):
                if isinstance(y, QLayout):
                    out += y.widgets()
                elif isinstance(y, QWidget):
                    out.append(y)
                    out += y.children_in_order()
        return out


class QVBoxLayout(QLayout):
    pass


class QHBoxLayout(QLayout):
    pass


class QFormLayout(QLayout):
    def addRow(self, label: Any, field: Any = None) -> None:  # noqa: N802
        if isinstance(label, str):
            label = QLabel(label)
        self._items.append((label, field) if field is not None else (label,))


# --- dialogs ---------------------------------------------------------------------

class QFileDialog:
    @staticmethod
    def getOpenFileName(parent=None, caption="", directory="", filter=""):  # noqa: N802, A002
        state.asked.append({"how": "open", "caption": caption, "directory": directory, "filter": filter})
        return state.next_filename, ""

    @staticmethod
    def getSaveFileName(parent=None, caption="", directory="", filter=""):  # noqa: N802, A002
        state.asked.append({"how": "save", "caption": caption, "directory": directory, "filter": filter})
        return state.next_filename, ""

    @staticmethod
    def getExistingDirectory(parent=None, caption="", directory=""):  # noqa: N802
        state.asked.append({"how": "dir", "caption": caption, "directory": directory})
        return state.next_filename


class QProgressDialog(QWidget):
    def __init__(self, label: str = "", cancel: str = "", lo: int = 0, hi: int = 100, parent: Any = None):
        super().__init__(parent)
        self.labels = [label]
        self.cancel_text = cancel
        self.values: List[int] = []
        self.title = ""
        self.closed = False
        state.progress.append(self)

    def setWindowTitle(self, t: str) -> None:  # noqa: N802
        self.title = t

    def setLabelText(self, t: str) -> None:  # noqa: N802
        self.labels.append(t)

    def setValue(self, v: int) -> None:  # noqa: N802
        self.values.append(int(v))

    def setMinimumDuration(self, ms: int) -> None:  # noqa: N802
        pass

    def setWindowModality(self, m: Any) -> None:  # noqa: N802
        pass

    def setAutoClose(self, on: bool) -> None:  # noqa: N802
        pass

    def setAutoReset(self, on: bool) -> None:  # noqa: N802
        pass

    def show(self) -> None:
        pass

    def wasCanceled(self) -> bool:  # noqa: N802
        return state.cancel

    def close(self) -> bool:
        self.closed = True
        return True

    def texts(self) -> List[str]:
        return self.labels + [self.title, self.cancel_text]


QtWidgets = types.ModuleType("QtWidgets")
for _c in (QWidget, QDialog, QLabel, QLineEdit, QPlainTextEdit, QComboBox, QPushButton, QToolButton, QCheckBox,
           QSpinBox, QDoubleSpinBox, QGroupBox, QVBoxLayout, QHBoxLayout, QFormLayout, QFileDialog, QProgressDialog):
    setattr(QtWidgets, _c.__name__, _c)


# --- the artist ---------------------------------------------------------------------

def type_text(w: Any, text: str) -> None:
    """The artist types into a line or text field."""
    assert w.isEnabled(), "greyed out"
    if isinstance(w, QPlainTextEdit):
        w.setPlainText(text)
        return
    w.setText(text)
    w.textEdited.emit(text)
    w.editingFinished.emit()


def click(w: QAbstractButton) -> bool:
    """The artist clicks a button or checkbox; False when it is greyed out."""
    if not w.isEnabled():
        return False
    if w._checkable:
        w.setChecked(not w.isChecked())
    w.clicked.emit(w.isChecked())
    return True


def choose(w: QComboBox, text: str) -> None:
    """The artist picks an item from a drop-down."""
    assert w.isEnabled(), "greyed out"
    i = w.findText(text)
    assert i >= 0, f"{text!r} is not one of {w.items()}"
    w.setCurrentIndex(i)
    w.activated.emit(i)


def spin(w: QAbstractSpinBox, v: Any) -> None:
    """The artist sets a number."""
    w.setValue(v)
