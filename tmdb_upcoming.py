import io
import json
import logging
import re
import urllib.request
from collections.abc import Iterable, Iterator
from datetime import date, datetime
from pathlib import Path
from typing import Any, Literal, TypedDict, TypeVar, Union, cast
from uuid import uuid4

import click
import lru_cache

logger = logging.getLogger("tmdb-upcoming")


@click.command()
@click.option("--people-file", type=click.File(mode="r"))
@click.option("--companies-file", type=click.File(mode="r"))
@click.option("--output-file", type=click.File(mode="w"), default="-")
@click.option("--api-key", type=str, envvar="TMDB_API_KEY", required=True)
@click.option(
    "--cache-file",
    envvar="CACHE_FILE",
    type=click.Path(writable=True, path_type=Path),
)
@click.option("--verbose", "-v", is_flag=True)
def main(
    people_file: io.TextIOWrapper | None,
    companies_file: io.TextIOWrapper | None,
    output_file: io.TextIOWrapper,
    api_key: str,
    cache_file: Path | None,
    verbose: bool,
) -> None:
    log_level = logging.DEBUG if verbose else logging.INFO
    logging.basicConfig(level=log_level)

    cache_max = 1000
    cache: lru_cache.LRUCache
    if cache_file:
        cache = lru_cache.open(cache_file, max_items=cache_max)
    else:
        cache = lru_cache.LRUCache(max_items=cache_max)

    people_ids = _read_ids(people_file)
    company_ids = _read_ids(companies_file)
    media_ids = _discover_credits(
        people_ids=people_ids,
        company_ids=company_ids,
        api_key=api_key,
    )

    feed: Feed = {
        "version": "https://jsonfeed.org/version/1.1",
        "title": "Movies in Production",
        "icon": "https://m.media-amazon.com/images/G/01/imdb/images-ANDW73HA/favicon_iPhone_retina_180x180._CB1582158069_.png",
        "items": [],
    }

    for media_type, media_id in _unique(media_ids):
        media = _media_object(media_type=media_type, media_id=media_id, api_key=api_key)

        title = ""
        if media["media_type"] == "movie":
            title = media["title"]
        elif media["media_type"] == "tv":
            title = media["name"]
        assert title

        imdb_id = media["external_ids"]["imdb_id"]
        if not imdb_id:
            logger.debug("Skip '%s' missing IMDb ID", title)
            continue

        in_production = (media["status"] == "In Production") or (
            media["status"] == "Post Production"
        )
        if not in_production:
            logger.debug("Skip '%s', not in production", title)
            continue
        first_seen_in_production: datetime = cache.get_or_load(
            f"first_seen_in_production:{imdb_id}", _now
        )

        imdb_url = f"https://www.imdb.com/title/{imdb_id}/"

        release_estimate = ""
        if media["media_type"] == "movie":
            if release_date := _parse_date(media["release_date"]):
                release_estimate = release_date.strftime("%B %Y")
        status = media["status"]

        details_updated: datetime = cache.get_or_load(
            f"details_updated:{imdb_id}:{title}:{status}:{release_estimate}", _now
        )

        content_text: str = ""
        if media["media_type"] == "movie":
            content_text = _movie_content_text(media, people_ids=people_ids)
        elif media["media_type"] == "tv":
            content_text = _tv_content_text(media, people_ids=people_ids)

        item_id: str = cache.get_or_load(f"item_id:{imdb_id}", lambda: str(uuid4()))

        item: Item = {
            "id": item_id,
            "url": imdb_url,
            "title": title,
            "content_text": content_text,
            "date_published": first_seen_in_production.isoformat(),
            "date_modified": details_updated.isoformat(),
        }
        feed["items"].append(item)

    feed["items"].sort(key=lambda item: item["id"])

    if isinstance(cache, lru_cache.PersistentLRUCache):
        cache.close()

    json.dump(feed, output_file, indent=4)


class Feed(TypedDict):
    version: Literal["https://jsonfeed.org/version/1.1"]
    title: str
    icon: str
    items: list["Item"]


class Item(TypedDict):
    id: str
    url: str
    title: str
    content_text: str
    date_published: str
    date_modified: str


def _read_ids(file: io.TextIOWrapper | None) -> set[int]:
    if not file:
        return set()
    return set(int(line.split("-", 1)[0]) for line in file)


_RELEVANT_DEPARTMENTS = set(["Directing", "Writing"])


def _discover_credits(
    api_key: str,
    people_ids: Iterable[int] = [],
    company_ids: Iterable[int] = [],
) -> Iterator[tuple[Literal["movie", "tv"], int]]:
    for person_id in people_ids:
        for credit in _person_credits(person_id, api_key=api_key):
            if credit["media_type"] == "movie" and credit["video"] is True:
                logger.debug("Skip video: %i", credit["id"])
                continue

            release_date = None
            if credit["media_type"] == "movie":
                release_date = _parse_date(credit["release_date"])
            elif credit["media_type"] == "tv":
                release_date = _parse_date(credit["first_air_date"])
            if release_date and release_date <= date.today():
                logger.debug("Skip already released: %i", credit["id"])
                continue

            if (
                credit["media_type"] == "movie"
                and credit["credit_type"] == "cast"
                and credit["order"] > 10
            ):
                logger.debug("Skip non-top billed credit: %i", credit["id"])
                continue

            if credit["credit_type"] == "cast" and _self_character(credit["character"]):
                logger.debug("Skip self credit: %i", credit["id"])
                continue

            if (
                credit["credit_type"] == "crew"
                and credit["department"] not in _RELEVANT_DEPARTMENTS
            ):
                logger.debug("Skip not relevant crew department: %i", credit["id"])
                continue

            if credit["media_type"] == "movie":
                yield "movie", credit["id"]
            elif credit["media_type"] == "tv":
                yield "tv", credit["id"]
            else:
                logger.warning(
                    "Unknown credit media type: %s",
                    credit["media_type"],
                )

    for company_id in company_ids:
        for movie in _discover_media_with_company(
            media_type="movie", company_id=company_id, api_key=api_key
        ):
            release_date = _parse_date(movie["release_date"])
            if release_date and release_date <= date.today():
                logger.debug("Skip already released: %i", movie["id"])
                continue
            yield "movie", movie["id"]


def _self_character(character: str) -> bool:
    return bool(
        re.search(
            string=character,
            pattern=r"self|himself|herself|uncredited|interviewee|archive footage",
            flags=re.IGNORECASE,
        )
    )


def _movie_content_text(media: "_Movie", people_ids: set[int]) -> str:
    content = f"\"{media['title']}\""

    director_name: str = "TBA"
    for crew in media["credits"]["crew"]:
        if crew["job"] == "Director":
            director_name = crew["name"]
            break
    content += f" directed by {director_name}"

    people_names = _relevant_people_names(media["credits"], people_ids)
    if director_name in people_names:
        people_names.remove(director_name)
    if people_names:
        content += f" along with {', '.join(people_names)}"
    content += "."

    release_date = _parse_date(media["release_date"])
    if release_date:
        content += f" Coming {release_date.strftime('%B %Y')}."

    return content


def _tv_content_text(media: "_TVShow", people_ids: set[int]) -> str:
    content = f"\"{media['name']}\""

    people_names = _relevant_people_names(media["credits"], people_ids)
    if people_names:
        content += f" with {', '.join(people_names)}"
    content += "."

    release_date = _parse_date(media["first_air_date"])
    if release_date:
        content += f" Coming {release_date.strftime('%B %Y')}."

    return content


def _relevant_people_names(credits: "_Credits", people_ids: set[int]) -> list[str]:
    names: set[str] = set()
    for cast_credit in credits["cast"]:
        if cast_credit["id"] in people_ids:
            names.add(cast_credit["name"])
    for crew_credit in credits["crew"]:
        if crew_credit["id"] in people_ids:
            names.add(crew_credit["name"])
    return sorted(list(names))


## TMDB API

_AnyMedia = Union["_Movie", "_TVShow"]


class _Movie(TypedDict):
    media_type: Literal["movie"]
    id: int
    title: str
    status: Literal[
        "Canceled",
        "In Production",
        "Planned",
        "Post Production",
        "Released",
        "Rumored",
    ]
    release_date: str
    credits: "_Credits"
    external_ids: "_ExternalIDs"


class _TVShow(TypedDict):
    media_type: Literal["tv"]
    id: int
    name: str
    first_air_date: str
    in_production: bool
    status: Literal[
        "Canceled",
        "Ended",
        "In Production",
        "Pilot",
        "Planned",
        "Returning Series",
    ]
    credits: "_Credits"
    external_ids: "_ExternalIDs"


class _ExternalIDs(TypedDict):
    imdb_id: str | None


class _Credits(TypedDict):
    cast: list["_CastCrew"]
    crew: list["_CrewCredit"]


class _CastCrew(TypedDict):
    id: int
    known_for_department: str
    name: str
    character: str
    order: int


class _CrewCredit(TypedDict):
    id: int
    known_for_department: str
    name: str
    department: str
    job: str


class _PersonCastMovieCredit(TypedDict):
    credit_type: Literal["cast"]
    media_type: Literal["movie"]
    id: int
    release_date: str
    video: bool
    character: str
    order: int


class _PersonCastTVCredit(TypedDict):
    credit_type: Literal["cast"]
    media_type: Literal["tv"]
    id: int
    first_air_date: str
    character: str


class _PersonCrewMovieCredit(TypedDict):
    credit_type: Literal["crew"]
    media_type: Literal["movie"]
    id: int
    release_date: str
    video: bool
    department: str
    job: str


class _PersonCrewTVCredit(TypedDict):
    credit_type: Literal["crew"]
    media_type: Literal["tv"]
    id: int
    first_air_date: str
    department: str
    job: str


_PersonMediaCredit = Union[
    "_PersonCastMovieCredit",
    "_PersonCastTVCredit",
    "_PersonCrewMovieCredit",
    "_PersonCrewTVCredit",
]


def _media_object(
    media_type: Literal["movie", "tv"],
    media_id: int,
    api_key: str,
) -> "_AnyMedia":
    url = f"https://api.themoviedb.org/3/{media_type}/{media_id}?append_to_response=credits,external_ids"
    obj = _get_json(url, api_key=api_key)
    obj["media_type"] = media_type
    return cast(_AnyMedia, obj)


def _person_credits(person_id: int, api_key: str) -> Iterator["_PersonMediaCredit"]:
    url = f"https://api.themoviedb.org/3/person/{person_id}/combined_credits"
    credits = _get_json(url, api_key=api_key)

    for credit in credits["cast"]:
        credit["credit_type"] = "cast"
        yield credit

    for credit in credits["crew"]:
        credit["credit_type"] = "crew"
        yield credit


def _discover_media_with_company(
    media_type: Literal["movie", "tv"],
    company_id: int,
    api_key: str,
) -> Iterator[dict[str, Any]]:  # TODO: define a type for this
    today: str = date.today().isoformat()
    url = f"https://api.themoviedb.org/3/discover/{media_type}?&release_date.gte={today}&sort_by=primary_release_date.asc&with_companies={company_id}"
    yield from _tmdb_get_paginated_json(url, api_key=api_key)


def _discover_media_with_person(
    media_type: Literal["movie", "tv"],
    person_id: int,
    api_key: str,
) -> Iterator[dict[str, Any]]:  # TODO: define a type for this
    today: str = date.today().isoformat()
    url = f"https://api.themoviedb.org/3/discover/{media_type}?&release_date.gte={today}&sort_by=primary_release_date.asc&with_people={person_id}"
    yield from _tmdb_get_paginated_json(url, api_key=api_key)


class _PaginatedJson(TypedDict):
    page: int
    results: list[Any]
    total_pages: int
    total_results: int


def _tmdb_get_paginated_json(url: str, api_key: str) -> Iterator[Any]:
    page = 1
    while True:
        data: _PaginatedJson = _get_json(f"{url}&page={page}", api_key=api_key)
        yield from data["results"]
        if data["page"] < data["total_pages"]:
            page += 1
            continue
        else:
            break


def _get_json(url: str, api_key: str) -> Any:
    logger.info("Fetch %s", url)
    headers = {"Accept": "application/json"}
    if "?" in url:
        url = f"{url}&api_key={api_key}"
    else:
        url = f"{url}?api_key={api_key}"
    req = urllib.request.Request(url, headers=headers)
    with urllib.request.urlopen(req, timeout=10) as response:
        data = response.read()
        assert isinstance(data, bytes)
        return json.loads(data)


# Utils

T = TypeVar("T")


def _unique(iterable: Iterable[T]) -> Iterator[T]:
    seen = set()
    for e in iterable:
        if e in seen:
            continue
        yield e
        seen.add(e)


def _now() -> datetime:
    return datetime.now().replace(microsecond=0)


def _parse_date(date_str: str | None) -> date | None:
    if not date_str:
        return None
    return date.fromisoformat(date_str)


if __name__ == "__main__":
    main()
