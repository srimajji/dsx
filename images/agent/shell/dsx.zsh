# DSX-managed interactive Zsh defaults. This file is self-contained and never reads host configuration.
[[ -o interactive ]] || return 0

# Keep the image-defined PATH intact while removing duplicate entries. Tool discovery
# remains an image concern; this file deliberately does not add language-specific paths.
typeset -gU path PATH
path=("${path[@]}")
export PATH

if (( ! ${+PAGER} )); then
  export PAGER=less
fi
if (( ! ${+EDITOR} )); then
  export EDITOR=vi
fi

setopt interactive_comments
setopt no_beep
setopt auto_cd
setopt append_history
setopt extended_history
setopt hist_expire_dups_first
setopt hist_find_no_dups
setopt hist_ignore_all_dups
setopt hist_ignore_space
setopt hist_reduce_blanks
setopt hist_save_no_dups
setopt hist_verify
setopt hist_fcntl_lock
setopt share_history

HISTFILE="${HOME}/.zsh_history"
HISTSIZE=20000
SAVEHIST=10000

# zsh-completions is loaded by the generated pre-loader so its functions are on
# fpath before compinit consumes the immutable, build-generated completion cache.
typeset -gr _DSX_SHELL_ROOT=/usr/local/share/dsx/shell
fpath=(/opt/antidote/functions "${fpath[@]}")
autoload -Uz antidote


if [[ ! -r "${_DSX_SHELL_ROOT}/plugins-pre.zsh" ]]; then
  print -u2 -- "dsx: missing managed Zsh pre-loader: ${_DSX_SHELL_ROOT}/plugins-pre.zsh"
  return 1
fi
source "${_DSX_SHELL_ROOT}/plugins-pre.zsh"

if [[ ! -r "${_DSX_SHELL_ROOT}/zcompdump" ]]; then
  print -u2 -- "dsx: missing managed completion cache: ${_DSX_SHELL_ROOT}/zcompdump"
  return 1
fi
autoload -Uz compinit
compinit -C -d "${_DSX_SHELL_ROOT}/zcompdump"

zstyle ':completion:*' completer _complete _ignored
zstyle ':completion:*' matcher-list 'm:{a-zA-Z}={A-Za-z}'
zstyle ':completion:*' menu select
zstyle ':completion:*' group-name ''
zstyle ':completion:*:descriptions' format '%d'
zstyle ':completion:*' squeeze-slashes true

# Load fzf's build-generated native key bindings and completion after compinit
# when ZLE has a terminal. The post-loader then gives fzf-tab the completion widget.
if [[ ! -r "${_DSX_SHELL_ROOT}/fzf-init.zsh" ]]; then
  print -u2 -- "dsx: missing managed fzf init: ${_DSX_SHELL_ROOT}/fzf-init.zsh"
  return 1
fi
if [[ -t 0 && -t 1 ]]; then
  source "${_DSX_SHELL_ROOT}/fzf-init.zsh"
fi

# fzf-tab is loaded first by the post-loader, followed by history search,
# autosuggestions, and finally syntax highlighting.
if [[ ! -r "${_DSX_SHELL_ROOT}/plugins-post.zsh" ]]; then
  print -u2 -- "dsx: missing managed Zsh post-loader: ${_DSX_SHELL_ROOT}/plugins-post.zsh"
  return 1
fi
source "${_DSX_SHELL_ROOT}/plugins-post.zsh"

zstyle ':fzf-tab:*' fzf-flags --height=50% --layout=reverse --border
zstyle ':fzf-tab:*' fzf-min-height 12
zstyle ':fzf-tab:*' switch-group '<' '>'
if [[ -n "${NO_COLOR:-}" ]]; then
  zstyle ':completion:*' list-colors ''
fi

# Bind both common cursor-key encodings in the interactive insert keymaps.
for _dsx_keymap in emacs viins; do
  bindkey -M "${_dsx_keymap}" '^[[A' history-substring-search-up
  bindkey -M "${_dsx_keymap}" '^[[B' history-substring-search-down
  bindkey -M "${_dsx_keymap}" '^[OA' history-substring-search-up
  bindkey -M "${_dsx_keymap}" '^[OB' history-substring-search-down
done
unset _dsx_keymap

alias ll='ls -alF'
alias la='ls -A'
alias l='ls -CF'
alias ..='cd ..'
alias ...='cd ../..'
alias ....='cd ../../..'
alias g='git'
alias gs='git status --short --branch'
alias gd='git diff'
alias gl='git log --graph --decorate --oneline -20'
alias reload='exec zsh -il'

mkcd() {
  emulate -L zsh
  if (( $# != 1 )); then
    print -u2 -- 'usage: mkcd DIR'
    return 2
  fi
  if ! mkdir -p -- "$1"; then
    print -u2 -- "mkcd: cannot create directory: $1"
    return 1
  fi
  if ! builtin cd -- "$1"; then
    print -u2 -- "mkcd: cannot enter directory: $1"
    return 1
  fi
}

extract() {
  emulate -L zsh
  if (( $# != 1 )); then
    print -u2 -- 'usage: extract FILE'
    return 2
  fi

  local archive="${1:A}"
  if [[ ! -f "${archive}" ]]; then
    print -u2 -- "extract: not a regular file: $1"
    return 1
  fi

  case "${archive:l}" in
    *.tar)
      (( ${+commands[tar]} )) || { print -u2 -- 'extract: tar is not installed'; return 127; }
      command tar --keep-old-files -xf "${archive}"
      ;;
    *.tar.gz|*.tgz)
      (( ${+commands[tar]} )) || { print -u2 -- 'extract: tar is not installed'; return 127; }
      command tar --keep-old-files -xzf "${archive}"
      ;;
    *.tar.bz2|*.tbz|*.tbz2)
      (( ${+commands[tar]} )) || { print -u2 -- 'extract: tar is not installed'; return 127; }
      command tar --keep-old-files -xjf "${archive}"
      ;;
    *.tar.xz|*.txz)
      (( ${+commands[tar]} )) || { print -u2 -- 'extract: tar is not installed'; return 127; }
      command tar --keep-old-files -xJf "${archive}"
      ;;
    *.zip)
      (( ${+commands[unzip]} )) || { print -u2 -- 'extract: unzip is not installed'; return 127; }
      command unzip -n "${archive}"
      ;;
    *.gz)
      (( ${+commands[gzip]} )) || { print -u2 -- 'extract: gzip is not installed'; return 127; }
      command gzip -dk -- "${archive}"
      ;;
    *)
      print -u2 -- "extract: unsupported archive type: $1"
      return 2
      ;;
  esac
}

serve() {
  emulate -L zsh
  if (( $# > 1 )); then
    print -u2 -- 'usage: serve [PORT]'
    return 2
  fi

  local port="${1:-8000}"
  if [[ "${port}" != <1-65535> ]]; then
    print -u2 -- "serve: port must be an integer from 1 to 65535: ${port}"
    return 2
  fi
  (( ${+commands[python]} )) || { print -u2 -- 'serve: python is not installed'; return 127; }
  command python -m http.server "${port}"
}

path() {
  emulate -L zsh
  if (( $# != 0 )); then
    print -u2 -- 'usage: path'
    return 2
  fi
  print -l -- "${path[@]}"
}

# Load the immutable direnv hook generated while building the image.
if [[ ! -r "${_DSX_SHELL_ROOT}/direnv-init.zsh" ]]; then
  print -u2 -- "dsx: missing managed direnv init: ${_DSX_SHELL_ROOT}/direnv-init.zsh"
  return 1
fi
source "${_DSX_SHELL_ROOT}/direnv-init.zsh"

export STARSHIP_CONFIG="${_DSX_SHELL_ROOT}/starship.toml"
if [[ ! -r "${_DSX_SHELL_ROOT}/starship-init.zsh" ]]; then
  print -u2 -- "dsx: missing managed Starship init: ${_DSX_SHELL_ROOT}/starship-init.zsh"
  return 1
fi
source "${_DSX_SHELL_ROOT}/starship-init.zsh"
