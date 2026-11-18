"""Generate rsrc_windows_amd64.syso: icon, manifest and version info for the exe.

The Go linker picks up any *.syso file in the package directory and links its
.rsrc section into WhatsApp.exe. This writes that COFF object directly, so the
build needs no extra tools (windres, rsrc, go-winres).

Usage:  python gen_resources.py [version]      e.g. python gen_resources.py 1.2.0
Reads icon.ico (run gen_icon.py first).
"""

import struct
import sys

ICON_GROUP_ID = 2   # must match iconResourceID in main.go
LANG_EN_US = 0x0409

RT_ICON, RT_GROUP_ICON, RT_VERSION, RT_MANIFEST = 3, 14, 16, 24

MANIFEST = """<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity version="{version}" processorArchitecture="*" name="Twincxt" type="win32"/>
  <description>Twincxt</description>
  <dependency>
    <dependentAssembly>
      <assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/>
    </dependentAssembly>
  </dependency>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}}"/>
    </application>
  </compatibility>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true/pm</dpiAware>
      <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">PerMonitorV2, PerMonitor</dpiAwareness>
    </windowsSettings>
  </application>
</assembly>
"""

STRINGS = {
    "CompanyName": "",
    "FileDescription": "Twincxt",
    "InternalName": "Twincxt",
    "OriginalFilename": "Twincxt.exe",
    "ProductName": "Twincxt",
    "LegalCopyright": "",
    "Comments": "WhatsApp Web and Telegram Web in one window. Not affiliated with WhatsApp, Meta or Telegram.",
}


def pad4(b):
    return b + b"\0" * (-len(b) % 4)


def utf16z(s):
    return (s + "\0").encode("utf-16-le")


# ---- icons -----------------------------------------------------------------

def read_ico(path):
    data = open(path, "rb").read()
    _, kind, count = struct.unpack_from("<HHH", data, 0)
    if kind != 1:
        raise ValueError(f"{path} is not an .ico file")
    images = []
    for i in range(count):
        w, h, colors, _, planes, bits, size, offset = struct.unpack_from("<BBBBHHII", data, 6 + 16 * i)
        images.append(((w, h, colors, planes, bits), data[offset:offset + size]))
    return images


def group_icon(images):
    out = struct.pack("<HHH", 0, 1, len(images))
    for i, ((w, h, colors, planes, bits), blob) in enumerate(images, start=1):
        out += struct.pack("<BBBBHHIH", w, h, colors, 0, planes, bits, len(blob), i)
    return out


# ---- version info ----------------------------------------------------------

def vs_block(key, value=b"", value_len=0, text=False, children=()):
    body = struct.pack("<HHH", 0, value_len, 1 if text else 0) + utf16z(key)
    body = pad4(body) + value
    if children:
        body = pad4(body) + b"".join(pad4(c) for c in children[:-1]) + children[-1]
    return struct.pack("<H", len(body)) + body[2:]


def version_info(version):
    parts = [int(p) for p in version.split(".")] + [0, 0, 0, 0]
    ms = (parts[0] << 16) | parts[1]
    ls = (parts[2] << 16) | parts[3]
    fixed = struct.pack(
        "<13I",
        0xFEEF04BD, 0x00010000,
        ms, ls, ms, ls,
        0x3F, 0,
        0x00040004,  # VOS_NT_WINDOWS32
        0x1,         # VFT_APP
        0, 0, 0,
    )
    strings = dict(STRINGS, FileVersion=version, ProductVersion=version)
    string_nodes = [
        vs_block(k, utf16z(v), len(v) + 1, text=True) for k, v in strings.items()
    ]
    string_table = vs_block("040904B0", text=True, children=string_nodes)
    string_file_info = vs_block("StringFileInfo", text=True, children=[string_table])
    translation = vs_block("Translation", struct.pack("<HH", LANG_EN_US, 1200), 4)
    var_file_info = vs_block("VarFileInfo", text=True, children=[translation])
    return vs_block("VS_VERSION_INFO", fixed, len(fixed), children=[string_file_info, var_file_info])


# ---- COFF object with a .rsrc section ---------------------------------------

def build_rsrc(resources):
    """resources: {type_id: {name_id: {lang_id: bytes}}} -> (section bytes, reloc offsets)."""
    types = sorted(resources)
    # Lay out: all directories first, then data entries, then raw data.
    dir_size = lambda n: 16 + 8 * n
    offset = dir_size(len(types))
    type_dirs = {}
    for t in types:
        type_dirs[t] = offset
        offset += dir_size(len(resources[t]))
    name_dirs = {}
    for t in types:
        for n in sorted(resources[t]):
            name_dirs[(t, n)] = offset
            offset += dir_size(len(resources[t][n]))
    leaves = [(t, n, l) for t in types for n in sorted(resources[t]) for l in sorted(resources[t][n])]
    entry_offs = {}
    for leaf in leaves:
        entry_offs[leaf] = offset
        offset += 16
    data_offs = {}
    for leaf in leaves:
        offset += -offset % 8
        data_offs[leaf] = offset
        offset += len(resources[leaf[0]][leaf[1]][leaf[2]])

    sec = bytearray(offset + (-offset % 8))

    def write_dir(at, ids_and_targets):
        struct.pack_into("<IIHHHH", sec, at, 0, 0, 0, 0, 0, len(ids_and_targets))
        for i, (ident, target) in enumerate(ids_and_targets):
            struct.pack_into("<II", sec, at + 16 + 8 * i, ident, target)

    write_dir(0, [(t, 0x80000000 | type_dirs[t]) for t in types])
    for t in types:
        write_dir(type_dirs[t], [(n, 0x80000000 | name_dirs[(t, n)]) for n in sorted(resources[t])])
        for n in sorted(resources[t]):
            write_dir(name_dirs[(t, n)], [(l, entry_offs[(t, n, l)]) for l in sorted(resources[t][n])])

    relocs = []
    for leaf in leaves:
        blob = resources[leaf[0]][leaf[1]][leaf[2]]
        # OffsetToData is an RVA; store the section offset and let the
        # linker add the section's RVA via an ADDR32NB relocation.
        struct.pack_into("<IIII", sec, entry_offs[leaf], data_offs[leaf], len(blob), 0, 0)
        relocs.append(entry_offs[leaf])
        sec[data_offs[leaf]:data_offs[leaf] + len(blob)] = blob
    return bytes(sec), relocs


def coff_object(section, relocs):
    IMAGE_FILE_MACHINE_AMD64 = 0x8664
    IMAGE_REL_AMD64_ADDR32NB = 3
    raw_ptr = 20 + 40
    reloc_ptr = raw_ptr + len(section)
    sym_ptr = reloc_ptr + 10 * len(relocs)

    header = struct.pack("<HHIIIHH", IMAGE_FILE_MACHINE_AMD64, 1, 0, sym_ptr, 1, 0, 0)
    sec_hdr = struct.pack(
        "<8sIIIIIIHHI",
        b".rsrc", 0, 0, len(section), raw_ptr, reloc_ptr, 0, len(relocs), 0,
        0x40000040,  # INITIALIZED_DATA | MEM_READ
    )
    reloc_bytes = b"".join(struct.pack("<IIH", r, 0, IMAGE_REL_AMD64_ADDR32NB) for r in relocs)
    symbol = struct.pack("<8sIhHBB", b".rsrc", 0, 1, 0, 3, 0)  # static section symbol
    string_table = struct.pack("<I", 4)
    return header + sec_hdr + section + reloc_bytes + symbol + string_table


def main():
    version = sys.argv[1] if len(sys.argv) > 1 else "1.0.0"
    version_full = ".".join((version.split(".") + ["0"] * 4)[:4])
    images = read_ico("icon.ico")

    resources = {
        RT_ICON: {i: {LANG_EN_US: blob} for i, (_, blob) in enumerate(images, start=1)},
        RT_GROUP_ICON: {ICON_GROUP_ID: {LANG_EN_US: group_icon(images)}},
        RT_VERSION: {1: {LANG_EN_US: version_info(version)}},
        RT_MANIFEST: {1: {LANG_EN_US: MANIFEST.format(version=version_full).encode("utf-8")}},
    }
    section, relocs = build_rsrc(resources)
    with open("rsrc_windows_amd64.syso", "wb") as f:
        f.write(coff_object(section, relocs))
    print(f"wrote rsrc_windows_amd64.syso ({len(images)} icon sizes, version {version})")


if __name__ == "__main__":
    main()
