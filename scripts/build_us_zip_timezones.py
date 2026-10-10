#!/usr/bin/env python3
"""Build the bounded US ZCTA-to-IANA lookup used for home ZIP edits.

Inputs are the Census 2026 ZCTA representative points and USDOT/BTS standard
time-zone polygons. This is an approximation of a USPS ZIP, so ambiguous
border points and Arizona are intentionally omitted rather than guessed.
Requires Shapely 2 only when regenerating this static file.
"""

import csv
import hashlib
import io
import json
from pathlib import Path
import urllib.request
import zipfile

from shapely.geometry import Point, shape


CENSUS = "https://www2.census.gov/geo/docs/maps-data/data/gazetteer/2026_Gazetteer/2026_Gaz_zcta_national.zip"
DOT = "https://geo.dot.gov/server/rest/services/hosted/timezone_polygons/FeatureServer/0/query?where=1%3D1&outFields=zone&outSR=4326&f=geojson"
OUTPUT = Path(__file__).resolve().parents[1] / "internal/account/data/us_zcta_timezones_2026.csv"
IANA = {
    "Atlantic": "America/Puerto_Rico",
    "Eastern": "America/New_York",
    "Central": "America/Chicago",
    "Mountain": "America/Denver",
    "Pacific": "America/Los_Angeles",
    "Alaska": "America/Anchorage",
}


def download(url):
    with urllib.request.urlopen(url, timeout=60) as response:
        data = response.read()
    print(url, hashlib.sha256(data).hexdigest())
    return data


def main():
    census = download(CENSUS)
    dot = json.loads(download(DOT))
    polygons = [(item["properties"]["zone"], shape(item["geometry"]))
                for item in dot["features"]]
    data = zipfile.ZipFile(io.BytesIO(census)).read("2026_Gaz_zcta_national.txt").decode()
    result = []
    for row in csv.DictReader(io.StringIO(data), delimiter="|"):
        zipcode = row["GEOID"]
        point = Point(float(row["INTPTLONG"]), float(row["INTPTLAT"]))
        zones = [zone for zone, polygon in polygons if polygon.covers(point)]
        if len(zones) != 1 or zones[0] not in IANA:
            continue
        # Arizona mostly does not observe daylight saving time. A ZIP alone
        # cannot establish the Navajo Nation exception, so omit AZ prefixes.
        if zipcode.startswith(("85", "86")):
            continue
        # A centroid near a standard-zone boundary is a weak ZIP assignment.
        polygon = next(p for zone, p in polygons if zone == zones[0])
        if polygon.boundary.distance(point) < 0.05:
            continue
        # Hawaii-Aleutian spans distinct IANA DST rules and is omitted.
        result.append((zipcode, IANA[zones[0]]))
    OUTPUT.parent.mkdir(parents=True, exist_ok=True)
    with OUTPUT.open("w", newline="") as target:
        writer = csv.writer(target)
        writer.writerow(("zip", "timezone"))
        writer.writerows(sorted(result))
    print(f"wrote {len(result)} supported ZCTAs to {OUTPUT}")


if __name__ == "__main__":
    main()
